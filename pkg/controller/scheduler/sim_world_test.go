package scheduler

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The simulated chain runs short epochs so that a run of a few hundred steps
// covers dozens of them.
const (
	simSecondsPerSlot = 6
	simSlotsPerEpoch  = 8
	simEpoch          = simSecondsPerSlot * simSlotsPerEpoch * time.Second
)

// Kinds of digests in the simulated registry.
const (
	digestGood = iota
	// digestFails: the container does not start; the agent reports failed.
	digestFails
	// digestMute: the container starts and the agent reports done, but the
	// validators never attest again.
	digestMute
)

const simPackage = "docker-ce"

// simContainer is one container of a simulated host.
type simContainer struct {
	name    string
	image   string
	managed bool
	custom  bool
	// running is the running digest, seen what the agent's last registry
	// poll saw.
	running   string
	seen      string
	checkedAt time.Time
}

// simManual is a need created by a roll or reboot directive.
type simManual struct {
	directive string
	kind      protocol.NeedKind
	container string
}

// simAgent is one simulated host with its agent. It speaks the protocol the
// way pkg/protocol describes it.
type simAgent struct {
	name     string
	val      *identity.ValidatorRange
	weight   uint64
	bootnode bool
	probes   bool
	verbs    []protocol.Verb

	containers []*simContainer
	// pkgInstalled and pkgSeen are versions of the blocked package.
	pkgInstalled   int
	pkgSeen        int
	rebootRequired bool
	boot           int

	localHold bool
	ctrlHold  bool
	manual    []simManual
	// handled remembers the answer to every directive ever applied; acks are
	// the answers to the directives of the last response.
	handled map[string]protocol.DirectiveAck
	acks    []protocol.DirectiveAck

	nextPoll    time.Time
	silentUntil time.Time
	outageUntil time.Time

	// lease is the persisted lease; deadline and startAt say until when and
	// when the agent starts it.
	lease    *protocol.Lease
	deadline time.Time
	startAt  time.Time
	// op is the operation on lease; it ends at opEnd with opFail deciding
	// the outcome. down is true while the operation has things stopped.
	op     *protocol.Operation
	opEnd  time.Time
	opFail bool
	reboot bool
	down   bool

	// World truth per lease ID, for the oracle: whether the agent stopped
	// anything, and when it finished successfully.
	disrupted map[string]bool
	doneAt    map[string]time.Time

	lastSync time.Time
	lastSent *protocol.SyncRequest
}

func (a *simAgent) allows(v protocol.Verb) bool { return slices.Contains(a.verbs, v) }

func (a *simAgent) held() bool { return a.localHold || a.ctrlHold }

func (a *simAgent) container(name string) *simContainer {
	for _, c := range a.containers {
		if c.name == name {
			return c
		}
	}

	return nil
}

// simWorld is the simulated fleet, registry and network. Nothing in it comes
// from the scheduler.
type simWorld struct {
	rng    *rand.Rand
	now    time.Time
	agents []*simAgent
	byName map[string]*simAgent

	images     []string
	registry   map[string]string
	digestKind map[string]int
	digests    int
	pkgVersion int

	// calm switches the random failures off (convergence).
	calm bool

	beaconDownUntil      time.Time
	finalityStallUntil   time.Time
	finalityStallSince   time.Time
	livenessStallUntil   time.Time
	livenessUnknownUntil time.Time
	dropUntil            time.Time
	dropPercent          uint64
	healthAt             time.Time

	// epoch is the epoch of now; epochMin the fewest validators each host
	// attested with at any moment of it.
	epoch    uint64
	epochMin map[string]uint64
	// published holds the liveness rows the beacon service stored, per
	// epoch; dropped names hosts whose stored rows were deleted since.
	published      map[uint64]map[string]uint64
	publishedEpoch uint64
	havePublished  bool
	dropped        map[string]bool
}

func simDigest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// newSimWorld builds a random fleet.
func newSimWorld(rng *rand.Rand) *simWorld {
	w := &simWorld{
		rng:        rng,
		now:        testGenesis.Add(50*simEpoch + 7*time.Second),
		byName:     make(map[string]*simAgent, 32),
		registry:   make(map[string]string, 4),
		digestKind: make(map[string]int, 16),
		epochMin:   make(map[string]uint64, 32),
		published:  make(map[uint64]map[string]uint64, 64),
		dropped:    make(map[string]bool, 2),
		pkgVersion: 1,
	}
	w.healthAt = w.now
	w.epoch = w.epochOf(w.now)

	for i := range 2 + rng.IntN(3) {
		image := fmt.Sprintf("org/client%d:devnet", i)
		w.images = append(w.images, image)
		w.registry[image] = w.newDigest(digestGood)
	}

	hosts := 3 + rng.IntN(10)
	if rng.IntN(3) == 0 {
		hosts = 3 + rng.IntN(28)
	}

	bootnodes := rng.IntN(3)
	next := uint64(0)

	for i := range hosts {
		a := &simAgent{
			name:         fmt.Sprintf("host-%02d", i),
			bootnode:     i < bootnodes,
			probes:       rng.IntN(10) < 7,
			verbs:        slices.Clone(allVerbs),
			pkgInstalled: 1,
			pkgSeen:      1,
			handled:      make(map[string]protocol.DirectiveAck, 8),
			disrupted:    make(map[string]bool, 8),
			doneAt:       make(map[string]time.Time, 8),
			localHold:    rng.IntN(25) == 0,
		}

		if rng.IntN(7) == 0 {
			a.verbs = slices.DeleteFunc(a.verbs, func(protocol.Verb) bool { return rng.IntN(2) == 0 })
		}

		hasValidators := rng.IntN(10) < 7
		if a.bootnode {
			hasValidators = rng.IntN(4) == 0
		}

		if hasValidators {
			a.weight = uint64(1 + rng.IntN(60))
			if rng.IntN(8) == 0 {
				a.weight = uint64(100 + rng.IntN(400))
			}

			a.val = &identity.ValidatorRange{Start: next, End: next + a.weight}
			next += a.weight
		}

		for c := range 1 + rng.IntN(3) {
			image := w.images[rng.IntN(len(w.images))]
			a.containers = append(a.containers, &simContainer{
				name:      fmt.Sprintf("c%d", c),
				image:     image,
				managed:   true,
				custom:    rng.IntN(12) == 0,
				running:   w.registry[image],
				seen:      w.registry[image],
				checkedAt: w.now,
			})
		}

		if rng.IntN(4) == 0 {
			a.containers = append(a.containers, &simContainer{
				name: "sidecar", image: "org/sidecar:latest", running: simDigest(1 << 20),
			})
		}

		a.nextPoll = w.now.Add(time.Duration(rng.IntN(60)) * time.Second)
		w.agents = append(w.agents, a)
		w.byName[a.name] = a
		w.epochMin[a.name] = w.attesting(a)
	}

	return w
}

func (w *simWorld) newDigest(kind int) string {
	w.digests++
	d := simDigest(w.digests)
	w.digestKind[d] = kind

	return d
}

func (w *simWorld) epochOf(t time.Time) uint64 { return uint64(t.Sub(testGenesis) / simEpoch) }

func (w *simWorld) epochStart(e uint64) time.Time {
	return testGenesis.Add(time.Duration(e) * simEpoch)
}

// describe renders the fleet for a failure report.
func (w *simWorld) describe() string {
	var b strings.Builder

	for _, a := range w.agents {
		fmt.Fprintf(&b, "  %s weight=%d bootnode=%v probes=%v verbs=%v localHold=%v:",
			a.name, a.weight, a.bootnode, a.probes, a.verbs, a.localHold)

		for _, c := range a.containers {
			fmt.Fprintf(&b, " %s=%s(managed=%v custom=%v)", c.name, c.image, c.managed, c.custom)
		}

		b.WriteByte('\n')
	}

	return b.String()
}

// attesting is how many of the host's validators attest right now.
func (w *simWorld) attesting(a *simAgent) uint64 {
	if a.weight == 0 || a.down || w.now.Before(a.outageUntil) {
		return 0
	}

	for _, c := range a.containers {
		if c.managed && !c.custom && w.digestKind[c.running] != digestGood {
			return 0
		}
	}

	if w.now.Before(w.dropUntil) {
		return a.weight - a.weight*w.dropPercent/100
	}

	return a.weight
}

// synced is what the host's probes say.
func (w *simWorld) synced(a *simAgent) bool {
	if a.down || w.now.Before(a.outageUntil) {
		return false
	}

	for _, c := range a.containers {
		if c.managed && !c.custom && w.digestKind[c.running] != digestGood {
			return false
		}
	}

	return true
}

// advance moves the clock, publishes the liveness of the epochs that ended
// and lets every agent work.
func (w *simWorld) advance(d time.Duration, st *fakeStore) {
	w.now = w.now.Add(d)

	for now := w.epochOf(w.now); w.epoch < now; w.epoch++ {
		w.publish(w.epoch, st)

		// Nothing changed since the last advance, so the next epoch starts
		// in the state the hosts are in.
		for _, a := range w.agents {
			w.epochMin[a.name] = w.attesting(a)
		}
	}

	for _, a := range w.agents {
		w.work(a)
	}

	w.sample()

	if !w.now.Before(w.beaconDownUntil) {
		w.healthAt = w.now
	}
}

// sample records the hosts' attesting validators for the running epoch.
func (w *simWorld) sample() {
	for _, a := range w.agents {
		w.epochMin[a.name] = min(w.epochMin[a.name], w.attesting(a))
	}
}

// publish stores the liveness rows of a complete epoch, unless the beacon
// service cannot get them.
func (w *simWorld) publish(epoch uint64, st *fakeStore) {
	end := w.epochStart(epoch + 1)
	if end.Before(w.beaconDownUntil) || end.Before(w.livenessStallUntil) || end.Before(w.livenessUnknownUntil) {
		return
	}

	rows := make(map[string]uint64, len(w.agents))
	stored := make([]fleet.Liveness, 0, len(w.agents))

	for _, a := range w.agents {
		// The beacon service tracks the validators of the hosts it knows.
		if a.weight == 0 || st.host(a.name) == nil {
			continue
		}

		rows[a.name] = w.epochMin[a.name]
		stored = append(stored, fleet.Liveness{
			Host: a.name, Epoch: epoch, Live: w.epochMin[a.name], Total: a.weight,
		})

		delete(w.dropped, a.name)
	}

	if len(stored) == 0 {
		return
	}

	w.published[epoch] = rows
	w.publishedEpoch = epoch
	w.havePublished = true

	st.putLiveness(stored...)
}

// publishedLive is the host's attesting validators in the newest published
// epoch as the store holds it; the full weight when there is no row.
func (w *simWorld) publishedLive(a *simAgent) uint64 {
	if !w.havePublished || w.dropped[a.name] {
		return a.weight
	}

	live, ok := w.published[w.publishedEpoch][a.name]
	if !ok {
		return a.weight
	}

	return min(live, a.weight)
}

// health is the beacon service's view.
func (w *simWorld) health() fleet.NetworkHealth {
	lag := uint64(2)
	if w.now.Before(w.finalityStallUntil) {
		lag += uint64(w.now.Sub(w.finalityStallSince) / simEpoch)
	}

	epoch := w.epochOf(w.healthAt)
	n := fleet.NetworkHealth{
		UpdatedAt:         w.healthAt,
		GenesisTime:       testGenesis,
		SecondsPerSlot:    simSecondsPerSlot,
		SlotsPerEpoch:     simSlotsPerEpoch,
		HeadSlot:          uint64(w.healthAt.Sub(testGenesis) / (simSecondsPerSlot * time.Second)),
		CurrentEpoch:      epoch,
		FinalizedEpoch:    epoch - lag,
		FinalityLagEpochs: lag,
		LivenessKnown:     w.havePublished && !w.now.Before(w.livenessUnknownUntil),
		LivenessEpoch:     w.publishedEpoch,
	}

	if w.now.Before(w.beaconDownUntil) {
		n.Error = "connection refused"
	}

	for _, a := range w.agents {
		if live, ok := w.published[w.publishedEpoch][a.name]; ok && w.havePublished {
			n.Tracked += a.weight
			n.Live += live
		}
	}

	return n
}

// work lets one agent act at the current time: poll its registries, start
// the lease it holds, move its operation on.
func (w *simWorld) work(a *simAgent) {
	if !w.now.Before(a.nextPoll) {
		w.poll(a)
	}

	if a.lease != nil && a.op == nil {
		switch {
		case !w.now.Before(a.deadline):
			// An agent that has not started when the lease expires must not
			// start.
			a.lease = nil
		case !w.now.Before(a.startAt):
			w.startOperation(a)
		}
	}

	if a.op == nil || a.op.Phase.Terminal() {
		return
	}

	if !w.now.Before(a.opEnd) {
		w.finishOperation(a)

		return
	}

	left := a.opEnd.Sub(w.now)
	total := a.opEnd.Sub(a.op.StartedAt)
	phase := protocol.PhaseVerifying

	switch {
	case left*10 > total*7 && a.reboot:
		phase = protocol.PhaseDraining
	case left*10 > total*5:
		phase = protocol.PhaseApplying
	case left*10 > total*4 && a.reboot:
		phase = protocol.PhaseRebooting
	case left*10 > total*2:
		phase = protocol.PhaseStarting
	}

	if a.op.Phase != phase {
		a.op.Phase = phase
		a.op.UpdatedAt = w.now
	}
}

// poll looks at the registries and the package candidates.
func (w *simWorld) poll(a *simAgent) {
	for _, c := range a.containers {
		if c.managed && !c.custom {
			c.seen = w.registry[c.image]
			c.checkedAt = w.now
		}
	}

	a.pkgSeen = w.pkgVersion
	a.nextPoll = w.now.Add(time.Duration(15+w.rng.IntN(90)) * time.Second)
}

// stepFor validates one granted need locally, as the agent must.
func (w *simWorld) stepFor(a *simAgent, n *protocol.Need) protocol.StepStatus {
	if a.held() || !a.allows(n.Kind.Verb()) {
		return protocol.StepRefused
	}

	switch n.Kind {
	case protocol.NeedImage:
		c := a.container(n.Image.Container)

		switch {
		case c == nil || !c.managed || c.custom:
			return protocol.StepRefused
		case c.image != n.Image.Image || w.registry[c.image] != n.Image.ToDigest:
			// The pulled digest must equal the lease's.
			return protocol.StepSuperseded
		case n.DirectiveID == "" && c.running == n.Image.ToDigest:
			return protocol.StepSuperseded
		}
	case protocol.NeedPackages:
		want := simPackages(a.pkgInstalled, w.pkgVersion)
		if a.pkgInstalled >= w.pkgVersion || protocol.PackagesNeedID(want) != n.ID {
			return protocol.StepSuperseded
		}
	case protocol.NeedReboot:
		if n.DirectiveID == "" && !a.rebootRequired {
			return protocol.StepSuperseded
		}
	}

	if n.DirectiveID != "" && !slices.ContainsFunc(a.manual, func(m simManual) bool {
		return m.directive == n.DirectiveID
	}) {
		return protocol.StepSuperseded
	}

	return protocol.StepRunning
}

// startOperation begins the work on the persisted lease.
func (w *simWorld) startOperation(a *simAgent) {
	op := &protocol.Operation{
		LeaseID:   a.lease.ID,
		Phase:     protocol.PhaseApplying,
		StartedAt: w.now,
		UpdatedAt: w.now,
		Steps:     make([]protocol.Step, 0, len(a.lease.Needs)),
	}

	a.reboot = false

	for i := range a.lease.Needs {
		n := &a.lease.Needs[i]
		status := w.stepFor(a, n)

		if status == protocol.StepRunning {
			op.Disrupted = true
			a.reboot = a.reboot || n.Kind == protocol.NeedReboot
		}

		op.Steps = append(op.Steps, protocol.Step{NeedID: n.ID, Status: status})
	}

	a.op = op

	if !op.Disrupted {
		op.Phase = protocol.PhaseDone

		return
	}

	if a.reboot {
		op.Phase = protocol.PhaseDraining
	}

	a.down = true
	a.disrupted[op.LeaseID] = true
	a.opEnd = w.now.Add(time.Duration(4+w.rng.IntN(80)) * time.Second)
	a.opFail = !w.calm && w.rng.IntN(25) == 0
}

// finishOperation ends the running operation and applies what it did.
func (w *simWorld) finishOperation(a *simAgent) {
	op := a.op
	op.UpdatedAt = w.now
	a.down = false

	if a.opFail {
		// Nothing was changed; the containers are back on what they ran.
		op.Phase = protocol.PhaseFailed
		op.Error = "apt-get exited with status 100"

		for i := range op.Steps {
			if op.Steps[i].Status == protocol.StepRunning {
				op.Steps[i].Status = protocol.StepFailed
				op.Steps[i].Error = op.Error
			}
		}

		return
	}

	op.Phase = protocol.PhaseDone

	for i := range op.Steps {
		step := &op.Steps[i]
		if step.Status != protocol.StepRunning {
			continue
		}

		step.Status = protocol.StepDone
		n := &a.lease.Needs[i]

		switch n.Kind {
		case protocol.NeedImage:
			c := a.container(n.Image.Container)
			c.running = n.Image.ToDigest

			if w.digestKind[c.running] == digestFails {
				step.Status = protocol.StepFailed
				step.Error = "container " + c.name + " exited"
				op.Phase = protocol.PhaseFailed
				op.Error = step.Error
			}
		case protocol.NeedPackages:
			a.pkgInstalled = w.pkgVersion
			a.pkgSeen = w.pkgVersion
			a.rebootRequired = a.rebootRequired || w.rng.IntN(4) == 0
		case protocol.NeedReboot:
			a.rebootRequired = false
			a.boot++
		}

		if n.DirectiveID != "" && step.Status == protocol.StepDone {
			a.manual = slices.DeleteFunc(a.manual, func(m simManual) bool {
				return m.directive == n.DirectiveID
			})
		}
	}

	if op.Phase == protocol.PhaseDone {
		a.doneAt[op.LeaseID] = w.now
	}
}

func simPackages(from, to int) []protocol.PackageChange {
	return []protocol.PackageChange{{
		Name: simPackage, From: fmt.Sprintf("1.%d", from), To: fmt.Sprintf("1.%d", to),
	}}
}

// needs lists the agent's open needs. A held agent reports none, and a need
// is reported only for a verb the local configuration allows.
func (w *simWorld) needs(a *simAgent) []protocol.Need {
	if a.held() {
		return nil
	}

	needs := make([]protocol.Need, 0, 4)

	if a.allows(protocol.VerbRoll) {
		for _, c := range a.containers {
			if !c.managed || c.custom || c.seen == c.running {
				continue
			}

			needs = append(needs, protocol.Need{
				ID:   protocol.ImageNeedID(c.name, c.seen),
				Kind: protocol.NeedImage,
				Image: &protocol.ImageNeed{
					Container: c.name, Image: c.image, FromDigest: c.running, ToDigest: c.seen,
				},
			})
		}
	}

	if a.allows(protocol.VerbPackages) && a.pkgSeen > a.pkgInstalled {
		pkgs := simPackages(a.pkgInstalled, a.pkgSeen)
		needs = append(needs, protocol.Need{
			ID:       protocol.PackagesNeedID(pkgs),
			Kind:     protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: pkgs},
		})
	}

	if a.allows(protocol.VerbReboot) && a.rebootRequired {
		needs = append(needs, rebootNeed())
	}

	// A manual need that local policy no longer permits is dropped.
	a.manual = slices.DeleteFunc(a.manual, func(m simManual) bool {
		if !a.allows(m.kind.Verb()) {
			return true
		}

		c := a.container(m.container)

		return m.kind == protocol.NeedImage && (c == nil || !c.managed || c.custom)
	})

	for _, m := range a.manual {
		n := protocol.Need{ID: protocol.ManualNeedID(m.directive), Kind: m.kind, DirectiveID: m.directive}

		if m.kind == protocol.NeedImage {
			c := a.container(m.container)
			n.Image = &protocol.ImageNeed{
				Container: c.name, Image: c.image, FromDigest: c.running, ToDigest: c.seen,
			}
		} else {
			n.Reboot = &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}}
		}

		needs = append(needs, n)
	}

	return needs
}

// request builds the agent's sync request.
func (w *simWorld) request(a *simAgent) *protocol.SyncRequest {
	req := &protocol.SyncRequest{
		ProtocolVersion: protocol.Version,
		AgentVersion:    "sim",
		SentAt:          w.now,
		BootID:          fmt.Sprintf("%s-boot-%d", a.name, a.boot),
		Verbs:           slices.Clone(a.verbs),
		Hold:            protocol.Hold{Local: a.localHold, Controller: a.ctrlHold},
		Containers:      make([]protocol.Container, 0, len(a.containers)),
		Needs:           w.needs(a),
		Operation:       cloneOperation(a.op),
		DirectiveAcks:   slices.Clone(a.acks),
	}

	for _, c := range a.containers {
		pc := protocol.Container{
			Name: c.name, Image: c.image, Digest: c.running, State: "running", Managed: c.managed,
		}

		if c.managed {
			pc.ConfiguredImage = c.image
		}

		if c.custom {
			pc.Custom = true
			pc.Image = "debug/" + c.image
			pc.Digest = ""
		}

		if c.managed && !c.custom {
			pc.Registry = &protocol.RegistryCheck{Digest: c.seen, CheckedAt: c.checkedAt, Pulled: true}
		}

		req.Containers = append(req.Containers, pc)
	}

	if a.probes {
		ok := w.synced(a)
		peers := 0

		if ok {
			peers = 12
		}

		req.Node = protocol.NodeHealth{
			EL: &protocol.ELHealth{Reachable: ok, Peers: peers},
			CL: &protocol.CLHealth{Reachable: ok, Peers: peers},
		}
	}

	return req
}

// respond lets the agent take in a sync response.
func (w *simWorld) respond(a *simAgent, resp *Assignment) {
	acks := make([]protocol.DirectiveAck, 0, len(resp.Directives))

	for _, d := range resp.Directives {
		ack, ok := a.handled[d.ID]
		if !ok {
			ack = w.apply(a, d)
			a.handled[d.ID] = ack
		}

		acks = append(acks, ack)
	}

	a.acks = acks
	busy := a.op != nil && !a.op.Phase.Terminal()

	switch {
	case resp.Lease == nil && busy:
		// Once started, the agent finishes.
	case resp.Lease == nil:
		// The controller no longer offers the lease: a finished operation is
		// forgotten, an unstarted lease dropped.
		a.op = nil
		a.lease = nil
	case a.lease != nil && a.lease.ID == resp.Lease.ID:
	case busy:
	default:
		l := *resp.Lease
		l.Needs = slices.Clone(resp.Lease.Needs)
		a.lease = &l
		a.op = nil
		a.deadline = w.now.Add(time.Duration(l.TTLSeconds) * time.Second)

		if l.ExpiresAt.Before(a.deadline) {
			a.deadline = l.ExpiresAt
		}

		delay := time.Duration(w.rng.IntN(8)) * time.Second

		switch n := w.rng.IntN(20); {
		case w.calm:
		case n == 0:
			delay = time.Hour
		case n < 4:
			delay = time.Duration(10+w.rng.IntN(60)) * time.Second
		}

		a.startAt = w.now.Add(delay)
	}
}

// apply executes one directive.
func (w *simWorld) apply(a *simAgent, d protocol.Directive) protocol.DirectiveAck {
	ack := protocol.DirectiveAck{ID: d.ID, Status: protocol.DirectiveAccepted}
	reject := func(reason string) protocol.DirectiveAck {
		ack.Status = protocol.DirectiveRejected
		ack.Error = reason

		return ack
	}

	switch d.Kind {
	case protocol.DirectiveHold:
		a.ctrlHold = true
	case protocol.DirectiveReleaseHold:
		a.ctrlHold = false
	case protocol.DirectiveRecheck:
		w.poll(a)
	case protocol.DirectiveRoll:
		c := a.container(d.Container)

		switch {
		case !a.allows(protocol.VerbRoll):
			return reject("verb roll is not allowed")
		case a.held():
			return reject("host is on hold")
		case c == nil || !c.managed:
			return reject("container is not managed")
		case c.custom:
			return reject("container runs a custom image")
		}

		a.manual = append(a.manual, simManual{directive: d.ID, kind: protocol.NeedImage, container: c.name})
	case protocol.DirectiveReboot:
		switch {
		case !a.allows(protocol.VerbReboot):
			return reject("verb reboot is not allowed")
		case a.held():
			return reject("host is on hold")
		}

		a.manual = append(a.manual, simManual{directive: d.ID, kind: protocol.NeedReboot})
	}

	return ack
}

// onTarget reports whether the host has nothing left to do that its
// configuration lets it do.
func (w *simWorld) onTarget(a *simAgent) error {
	if a.allows(protocol.VerbRoll) {
		for _, c := range a.containers {
			if c.managed && !c.custom && c.running != w.registry[c.image] {
				return fmt.Errorf("%s: container %s runs %s, the tag points at %s",
					a.name, c.name, shortDigest(c.running), shortDigest(w.registry[c.image]))
			}
		}
	}

	if a.allows(protocol.VerbPackages) && a.pkgInstalled != w.pkgVersion {
		return fmt.Errorf("%s: package version 1.%d installed, 1.%d available", a.name, a.pkgInstalled, w.pkgVersion)
	}

	if a.allows(protocol.VerbReboot) && a.rebootRequired {
		return fmt.Errorf("%s: reboot still required", a.name)
	}

	return nil
}
