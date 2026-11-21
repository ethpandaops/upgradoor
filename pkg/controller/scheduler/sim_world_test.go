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

// simLatencySeconds bounds how far an agent's start deadline lies after the
// lease's expiry. It must stay below leaseStartGrace.
const simLatencySeconds = 20

// simContainer is one container of a simulated host.
type simContainer struct {
	name    string
	image   string
	managed bool
	custom  bool
	// impact is the class of a managed container. Only work on node
	// containers takes the node down.
	impact protocol.Impact
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
	name   string
	val    *identity.ValidatorRange
	weight uint64
	probes bool
	// peerMax is how many peers the host's clients have once they found them
	// all again. It follows from the host's number, so that no seed draws a
	// different history for it.
	peerMax int
	verbs   []protocol.Verb

	containers []*simContainer
	// pkgInstalled and pkgSeen are versions of the blocked package.
	pkgInstalled   int
	pkgSeen        int
	rebootRequired bool
	// rebootSticks: the host comes back from a reboot still requiring one
	// (its boot configuration picks the old kernel).
	rebootSticks bool
	boot         int

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
	// upAt is when the last operation ended: the node finds its peers again
	// from then on.
	upAt time.Time

	// What the node goes through besides being up or down. syncingUntil: a
	// client reports syncing (the execution client when syncingEL, else the
	// consensus client). elOfflineUntil: the consensus client reports its
	// execution client offline. elDownUntil: the execution client does not
	// answer. peerCapUntil: the node has at most peerCap peers. elLag: the
	// execution client finds its peers that much later than the consensus
	// client.
	// dead: the host's validators never attest, while its node and its agent
	// are fine (a broken validator client, missing keys).
	dead bool
	// sloppy: the agent reports its needs also while it is held and for verbs
	// its configuration does not allow. It refuses the steps all the same.
	sloppy bool
	// blind: the agent's registry polls see nothing new, so it never reports
	// the need the controller expects of it.
	blind bool
	// overrun: the agent's clock is slow, so that it may start its lease
	// after the controller has expired it.
	overrun bool
	// stepFail is the index of the step of the running operation that fails
	// while the others succeed; -1 for none.
	stepFail int
	// beginAt is, per step of the running operation, when the step begins;
	// the zero time for a step that is not executed. The steps of an operation
	// begin one after the other.
	beginAt []time.Time
	// cancelled: the agent has taken in that its lease was cancelled. It
	// begins no further step.
	cancelled bool

	syncingUntil   time.Time
	syncingEL      bool
	elOfflineUntil time.Time
	elDownUntil    time.Time
	peerCapUntil   time.Time
	peerCap        int
	elLag          time.Duration

	// late names the leases the agent started with a slow clock: possibly
	// after the controller had expired them.
	late map[string]bool
	// begins is every step the agent ever began, in order; cancelSeen is, per
	// lease, when the agent took in that the lease was cancelled or withdrawn.
	// An old build takes in neither.
	begins     []simBegin
	cancelSeen map[string]time.Time

	lastSync time.Time
	lastSent *protocol.SyncRequest
	// reported is, per lease ID, the operation the agent sent to the
	// controller for the lease, up to the first report that it is over: its
	// own record of what it said.
	reported map[string]*protocol.Operation
}

// simBegin is one step that an agent began: the first disruptive thing it
// did for the need.
type simBegin struct {
	lease, need string
	at          time.Time
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
	rng *rand.Rand
	now time.Time
	// start is when the world began.
	start  time.Time
	agents []*simAgent
	byName map[string]*simAgent

	images     []string
	registry   map[string]string
	digestKind map[string]int
	digests    int
	pkgVersion int

	// calm switches the random failures off (convergence).
	calm               bool
	beaconDownUntil    time.Time
	finalityStallUntil time.Time
	finalityStallSince time.Time
	// blackoutUntil: no validator of any host is seen attesting.
	blackoutUntil        time.Time
	livenessStallUntil   time.Time
	livenessUnknownUntil time.Time
	dropUntil            time.Time
	dropPercent          uint64
	healthAt             time.Time

	// phases are the chain's slot durations: the first from genesis on, one
	// more for every fork that changes the duration.
	phases []simPhase

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

// simImpacts are the classes a managed container is drawn from: mostly the
// node's own containers.
var simImpacts = []protocol.Impact{
	protocol.ImpactNode, protocol.ImpactNode, protocol.ImpactNode, protocol.ImpactSlot, protocol.ImpactIndependent,
}

// simLeaseImpact is the world's own class of the work of a lease: node when
// any need is about the node or a node container.
func simLeaseImpact(a *simAgent, needs []protocol.Need) protocol.Impact {
	impact := protocol.ImpactIndependent

	for i := range needs {
		c := a.container(needs[i].Container())
		if c == nil || !c.managed {
			return protocol.ImpactNode
		}

		impact = impact.Heavier(c.impact)
	}

	return impact
}

func simDigest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// simStart is when a simulated world begins, unless its chain has not begun
// yet (newSim).
var simStart = testGenesis.Add(50*simEpoch + 7*time.Second)

// newSimWorld builds a random fleet at a time, which may lie before the
// chain's genesis.
func newSimWorld(rng *rand.Rand, start time.Time) *simWorld {
	w := &simWorld{
		rng:        rng,
		now:        start,
		start:      start,
		byName:     make(map[string]*simAgent, 32),
		registry:   make(map[string]string, 4),
		digestKind: make(map[string]int, 16),
		epochMin:   make(map[string]uint64, 32),
		published:  make(map[uint64]map[string]uint64, 64),
		dropped:    make(map[string]bool, 2),
		pkgVersion: 1,
		phases:     []simPhase{{start: testGenesis, slot: simSecondsPerSlot * time.Second}},
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

	next := uint64(0)

	for i := range hosts {
		a := &simAgent{
			name:         fmt.Sprintf("host-%02d", i),
			probes:       rng.IntN(10) < 7,
			peerMax:      simPeers + i%simPeerSpread,
			verbs:        slices.Clone(allVerbs),
			pkgInstalled: 1,
			pkgSeen:      1,
			handled:      make(map[string]protocol.DirectiveAck, 8),
			late:         make(map[string]bool, 2),
			reported:     make(map[string]*protocol.Operation, 8),
			cancelSeen:   make(map[string]time.Time, 2),
			localHold:    rng.IntN(25) == 0,
			dead:         rng.IntN(20) == 0,
			sloppy:       rng.IntN(5) == 0,
			blind:        rng.IntN(12) == 0,
			stepFail:     -1,
		}

		if rng.IntN(7) == 0 {
			a.verbs = slices.DeleteFunc(a.verbs, func(protocol.Verb) bool { return rng.IntN(2) == 0 })
		}

		if rng.IntN(10) < 7 {
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
				name:    fmt.Sprintf("c%d", c),
				image:   image,
				managed: true,
				custom:  rng.IntN(12) == 0,
				// From the host and the container, not from the random
				// stream, so that the seeds keep their histories.
				impact:    simImpacts[(i+2*c)%len(simImpacts)],
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

// simPhase is a stretch of the simulated chain with one slot duration.
type simPhase struct {
	epoch uint64
	start time.Time
	slot  time.Duration
}

// phaseOf returns the phase an epoch is in.
func (w *simWorld) phaseOf(e uint64) simPhase {
	phase := w.phases[0]

	for _, next := range w.phases[1:] {
		if next.epoch <= e {
			phase = next
		}
	}

	return phase
}

// epochOf is the epoch a time is in. This is the world's own clock: it does
// not share code with the scheduler's. Before genesis no epoch has begun:
// the time counts to the first one, which is not complete before it has run.
func (w *simWorld) epochOf(t time.Time) uint64 {
	phase := w.phases[0]
	if t.Before(phase.start) {
		return phase.epoch
	}

	for _, next := range w.phases[1:] {
		if !t.Before(next.start) {
			phase = next
		}
	}

	return phase.epoch + uint64(t.Sub(phase.start)/(simSlotsPerEpoch*phase.slot))
}

// epochStart is when an epoch begins.
func (w *simWorld) epochStart(e uint64) time.Time {
	phase := w.phaseOf(e)

	return phase.start.Add(time.Duration(e-phase.epoch) * simSlotsPerEpoch * phase.slot)
}

// fork schedules a change of the slot duration at an epoch that has not
// begun. The chain has at most two of them.
func (w *simWorld) fork(at uint64, slot time.Duration) bool {
	last := w.phases[len(w.phases)-1]
	if len(w.phases) > 2 || at <= w.epochOf(w.now) || at <= last.epoch || slot == last.slot {
		return false
	}

	w.phases = append(w.phases, simPhase{epoch: at, start: w.epochStart(at), slot: slot})

	return true
}

// describe renders the fleet for a failure report.
func (w *simWorld) describe() string {
	var b strings.Builder

	for _, a := range w.agents {
		fmt.Fprintf(&b, "  %s weight=%d probes=%v peers=%d verbs=%v localHold=%v dead=%v sloppy=%v blind=%v:",
			a.name, a.weight, a.probes, a.peerMax, a.verbs, a.localHold, a.dead, a.sloppy, a.blind)

		for _, c := range a.containers {
			fmt.Fprintf(&b, " %s=%s(managed=%v custom=%v)", c.name, c.image, c.managed, c.custom)
		}

		b.WriteByte('\n')
	}

	return b.String()
}

// attesting is how many of the host's validators attest right now. A node
// that is syncing, has lost its execution client or has no peer attests with
// none.
func (w *simWorld) attesting(a *simAgent) uint64 {
	if a.weight == 0 || a.dead || w.now.Before(w.blackoutUntil) || !w.synced(a) || w.behind(a) ||
		w.clPeers(a) == 0 {
		return 0
	}

	if w.now.Before(w.dropUntil) {
		return a.weight - a.weight*w.dropPercent/100
	}

	return a.weight
}

// simPeers is how many peers a node has at least once it found them all
// again, and simPeerSpread how many values that number takes over the hosts
// (12 to 16: the fewest are three quarters of the most, so that a host at its
// full number is never below the largest readyPeerShare of a run);
// simPeerEvery is how long each of them takes after the node came back.
const (
	simPeers      = 12
	simPeerSpread = 5
	simPeerEvery  = 3 * time.Second
)

// clPeers is how many peers the consensus client has: none while the node is
// down, then one more every simPeerEvery since it came back from its last
// operation or outage, and no more than peerCap while the node has trouble
// keeping them.
func (w *simWorld) clPeers(a *simAgent) int {
	return w.peersAfter(a, 0)
}

// elPeers is the same for the execution client, which finds its peers elLag
// later and has none while it does not answer.
func (w *simWorld) elPeers(a *simAgent) int {
	if w.now.Before(a.elDownUntil) {
		return 0
	}

	return w.peersAfter(a, a.elLag)
}

func (w *simWorld) peersAfter(a *simAgent, lag time.Duration) int {
	if !w.synced(a) {
		return 0
	}

	peers := int(min(time.Duration(a.peerMax), max(0, w.now.Sub(later(a.upAt, a.outageUntil))-lag)/simPeerEvery))
	if w.now.Before(a.peerCapUntil) {
		peers = min(peers, a.peerCap)
	}

	return peers
}

// synced reports whether the host's node is up: no operation has it stopped,
// it is in no outage and its containers run.
func (w *simWorld) synced(a *simAgent) bool {
	if a.down || w.now.Before(a.outageUntil) {
		return false
	}

	for _, c := range a.containers {
		if c.managed && !c.custom && c.impact == protocol.ImpactNode && w.digestKind[c.running] != digestGood {
			return false
		}
	}

	return true
}

// behind reports whether a node that is up is not at head: a client is
// syncing, or the consensus client has lost its execution client.
func (w *simWorld) behind(a *simAgent) bool {
	return w.now.Before(a.syncingUntil) || w.now.Before(a.elOfflineUntil) || w.now.Before(a.elDownUntil)
}

// node is what the host's probes say.
func (w *simWorld) node(a *simAgent) protocol.NodeHealth {
	var (
		up      = w.synced(a)
		syncing = up && w.now.Before(a.syncingUntil)
		elUp    = up && !w.now.Before(a.elDownUntil)
	)

	return protocol.NodeHealth{
		EL: &protocol.ELHealth{Reachable: elUp, Syncing: syncing && a.syncingEL && elUp, Peers: w.elPeers(a)},
		CL: &protocol.CLHealth{
			Reachable: up,
			Syncing:   syncing && !a.syncingEL,
			ELOffline: up && (!elUp || w.now.Before(a.elOfflineUntil)),
			Peers:     w.clPeers(a),
		},
	}
}

// comeBack decides what the node goes through after an operation: now and
// then it has to catch up first, its execution client takes longer than its
// consensus client, or it keeps losing its peers.
func (w *simWorld) comeBack(a *simAgent) {
	a.upAt = w.now
	a.elLag = 0

	if w.calm {
		return
	}

	if w.rng.IntN(3) == 0 {
		a.syncingUntil = w.now.Add(time.Duration(2+w.rng.IntN(40)) * time.Second)
		a.syncingEL = w.rng.IntN(2) == 0
	}

	if w.rng.IntN(5) == 0 {
		a.elOfflineUntil = w.now.Add(time.Duration(2+w.rng.IntN(30)) * time.Second)
	}

	if w.rng.IntN(3) == 0 {
		a.elLag = time.Duration(3+w.rng.IntN(40)) * time.Second
	}
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

// storedLiveness reports whether the store still holds a row of the newest
// published epoch: the rows of a forgotten host and of a lost database are
// gone until the next epoch is published.
func (w *simWorld) storedLiveness() bool {
	if !w.havePublished {
		return false
	}

	for name := range w.published[w.publishedEpoch] {
		if !w.dropped[name] {
			return true
		}
	}

	return false
}

// health is the beacon service's view.
func (w *simWorld) health() fleet.NetworkHealth {
	lag := uint64(2)
	if w.now.Before(w.finalityStallUntil) {
		lag += uint64(w.now.Sub(w.finalityStallSince) / simEpoch)
	}

	epoch := w.epochOf(w.healthAt)
	phase := w.phaseOf(epoch)

	// A chain that has just begun (or has not yet) has finalized nothing, and
	// lags by no more than it is old.
	lag = min(lag, epoch)

	slot := uint64(0)
	if since := w.healthAt.Sub(w.epochStart(epoch)); since > 0 {
		slot = uint64(since / phase.slot)
	}
	phases := make([]fleet.SlotPhase, 0, len(w.phases))

	// The beacon service publishes the whole schedule, forks to come too.
	for _, p := range w.phases {
		phases = append(phases, fleet.SlotPhase{Epoch: p.epoch, Start: p.start, SlotMs: uint64(p.slot / time.Millisecond)})
	}

	n := fleet.NetworkHealth{
		UpdatedAt:         w.healthAt,
		GenesisTime:       testGenesis,
		SecondsPerSlot:    uint64(phase.slot / time.Second),
		Phases:            phases,
		SlotsPerEpoch:     simSlotsPerEpoch,
		HeadSlot:          epoch*simSlotsPerEpoch + slot,
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

	w.beginSteps(a)

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
		if c.managed && !c.custom && !a.blind {
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
	case protocol.NeedReboot, protocol.NeedOverride:
		if n.DirectiveID == "" && !a.rebootRequired {
			return protocol.StepSuperseded
		}
	case protocol.NeedSpec:
		// The simulated agents have no agent.yaml that changes.
		return protocol.StepRefused
	}

	if n.DirectiveID != "" && !slices.ContainsFunc(a.manual, func(m simManual) bool {
		return m.directive == n.DirectiveID
	}) {
		return protocol.StepSuperseded
	}

	return protocol.StepRunning
}

// startOperation begins the work on the persisted lease. Its steps begin one
// after the other in the first half of the operation; the first begins now.
func (w *simWorld) startOperation(a *simAgent) {
	op := &protocol.Operation{
		LeaseID:   a.lease.ID,
		Phase:     protocol.PhaseApplying,
		StartedAt: w.now,
		UpdatedAt: w.now,
		Steps:     make([]protocol.Step, 0, len(a.lease.Needs)),
	}

	a.reboot = false
	a.stepFail = -1
	a.beginAt = make([]time.Time, len(a.lease.Needs))

	runs := make([]int, 0, len(a.lease.Needs))

	for i := range a.lease.Needs {
		n := &a.lease.Needs[i]
		status := w.stepFor(a, n)

		if status == protocol.StepRunning {
			status = protocol.StepPending
			op.Disrupted = true
			a.reboot = a.reboot || n.Kind == protocol.NeedReboot

			runs = append(runs, i)
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

	// Work on slot and independent containers leaves the node up.
	a.down = simLeaseImpact(a, a.lease.Needs) == protocol.ImpactNode

	if a.overrun {
		a.late[op.LeaseID] = true
	}

	a.opEnd = w.now.Add(time.Duration(4+w.rng.IntN(80)) * time.Second)
	a.opFail = !w.calm && w.rng.IntN(25) == 0

	// Now and then an operation takes long: longer than any leaseTtl. Its
	// lease stays open for as long as the agent reports.
	if !w.calm && w.rng.IntN(12) == 0 {
		a.opEnd = w.now.Add(time.Duration(330+w.rng.IntN(600)) * time.Second)
	}

	for k, i := range runs {
		a.beginAt[i] = w.now.Add(a.opEnd.Sub(w.now) / 2 * time.Duration(k) / time.Duration(len(runs)))
	}

	// Now and then one step of several fails while the others succeed.
	if !w.calm && len(runs) > 1 && w.rng.IntN(4) == 0 {
		a.stepFail = runs[w.rng.IntN(len(runs))]
	}

	w.beginSteps(a)
}

// beginSteps begins the steps of the running operation whose time has come.
// An agent that has taken in the cancel of its lease begins none.
func (w *simWorld) beginSteps(a *simAgent) {
	for i := range a.op.Steps {
		step := &a.op.Steps[i]
		if step.Status != protocol.StepPending || a.beginAt[i].IsZero() || w.now.Before(a.beginAt[i]) {
			continue
		}

		if a.cancelled {
			continue
		}

		step.Status = protocol.StepRunning
		a.op.UpdatedAt = w.now
		a.begins = append(a.begins, simBegin{lease: a.op.LeaseID, need: step.NeedID, at: w.now})
	}
}

// decline ends a lease that the agent has not started and never will: it was
// cancelled, or the controller no longer offers it. Nothing is touched, and
// the agent says so.
func (w *simWorld) decline(a *simAgent) {
	op := &protocol.Operation{
		LeaseID:   a.lease.ID,
		Phase:     protocol.PhaseDone,
		StartedAt: w.now,
		UpdatedAt: w.now,
		Steps:     make([]protocol.Step, 0, len(a.lease.Needs)),
	}

	for i := range a.lease.Needs {
		op.Steps = append(op.Steps, protocol.Step{
			NeedID: a.lease.Needs[i].ID, Status: protocol.StepRefused, Error: "the lease is not to be started",
		})
	}

	a.op = op
	a.cancelSeen[op.LeaseID] = w.now
}

// cancel lets an agent take in that its lease was cancelled: one that has not
// started declines it, one that has begins no further step.
func (w *simWorld) cancel(a *simAgent) {
	if a.cancelled || (a.op != nil && a.op.Phase.Terminal()) {
		return
	}

	a.cancelled = true

	if a.op == nil {
		w.decline(a)

		return
	}

	a.cancelSeen[a.op.LeaseID] = w.now
}

// finishOperation ends the running operation and applies what it did.
func (w *simWorld) finishOperation(a *simAgent) {
	op := a.op
	op.UpdatedAt = w.now

	if a.down {
		a.down = false
		w.comeBack(a)
	}

	// A step that had not begun when the agent took in the cancel never
	// began. How it is reported is settled when the operation's end is known.
	defer w.reportUnbegun(a, op)

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
	rebooted := false

	for i := range op.Steps {
		step := &op.Steps[i]
		if step.Status != protocol.StepRunning {
			continue
		}

		if i == a.stepFail {
			step.Status = protocol.StepFailed
			step.Error = "step exited with status 1"
			op.Phase = protocol.PhaseFailed
			op.Error = step.Error

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
		case protocol.NeedOverride, protocol.NeedSpec:
			// The simulated agents have no overrides file and no agent.yaml
			// that changes.
		case protocol.NeedReboot:
			rebooted = true
			a.boot++
		}

		if n.DirectiveID != "" && step.Status == protocol.StepDone {
			a.manual = slices.DeleteFunc(a.manual, func(m simManual) bool {
				return m.directive == n.DirectiveID
			})
		}
	}

	// The reboot is the last thing an operation does, so it also covers
	// what the package upgrade of the same operation asked for.
	if rebooted {
		a.rebootRequired = a.rebootSticks
	}
}

// reportUnbegun gives the steps that never began (the agent had taken in the
// cancel of its lease) their final status: cancelled when the cancel is what
// ended the operation; after a failure, as after any failure (pending, not
// attempted).
func (w *simWorld) reportUnbegun(a *simAgent, op *protocol.Operation) {
	failed := op.Phase == protocol.PhaseFailed

	for i := range op.Steps {
		step := &op.Steps[i]
		if step.Status != protocol.StepPending || a.beginAt[i].IsZero() {
			continue
		}

		if failed {
			step.Error = "not attempted: " + op.Error

			continue
		}

		step.Status = protocol.StepCancelled
		step.Error = "cancelled before it had begun"
	}
}

func simPackages(from, to int) []protocol.PackageChange {
	return []protocol.PackageChange{{
		Name: simPackage, From: fmt.Sprintf("1.%d", from), To: fmt.Sprintf("1.%d", to),
	}}
}

// needs lists the agent's open needs. A held agent reports none, and a need
// is reported only for a verb the local configuration allows; a sloppy agent
// reports its observed needs regardless.
func (w *simWorld) needs(a *simAgent) []protocol.Need {
	if a.held() && !a.sloppy {
		return nil
	}

	needs := make([]protocol.Need, 0, 4)

	if a.allows(protocol.VerbRoll) || a.sloppy {
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

	if (a.allows(protocol.VerbPackages) || a.sloppy) && a.pkgSeen > a.pkgInstalled {
		pkgs := simPackages(a.pkgInstalled, a.pkgSeen)
		needs = append(needs, protocol.Need{
			ID:       protocol.PackagesNeedID(pkgs),
			Kind:     protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: pkgs},
		})
	}

	if (a.allows(protocol.VerbReboot) || a.sloppy) && a.rebootRequired {
		needs = append(needs, rebootNeed())
	}

	if a.held() {
		// Held: the observed needs at most, no manual ones.
		return needs
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
			pc.Impact = c.impact
		}

		if c.custom {
			pc.Image = "debug/" + c.image
			pc.Digest = ""
		}

		if c.managed && !c.custom {
			pc.Registry = &protocol.RegistryCheck{Digest: c.seen, CheckedAt: c.checkedAt, Pulled: true}
		}

		req.Containers = append(req.Containers, pc)
	}

	if a.probes {
		req.Node = w.node(a)
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
	case resp.Lease == nil && a.lease != nil && a.op == nil:
		// The controller no longer offers the lease and nothing was done for
		// it: it is not started any more, and the agent says so.
		w.decline(a)
	case resp.Lease == nil:
		// The controller no longer offers the lease: a finished operation is
		// forgotten.
		a.op = nil
		a.lease = nil
		a.cancelled = false
	case a.lease != nil && a.lease.ID == resp.Lease.ID:
		if resp.Lease.Cancel {
			w.cancel(a)
		}
	case busy:
	case resp.Lease.Cancel:
		// A lease that is offered cancelled is not taken.
	default:
		l := *resp.Lease
		l.Needs = slices.Clone(resp.Lease.Needs)
		a.lease = &l
		a.op = nil
		a.cancelled = false
		a.deadline = w.now.Add(time.Duration(l.TTLSeconds) * time.Second)

		if l.ExpiresAt.Before(a.deadline) {
			a.deadline = l.ExpiresAt
		}

		// The response took a while to arrive and the agent's clock is not
		// the controller's: its deadline lies a little after the lease's.
		a.deadline = a.deadline.Add(time.Duration(w.rng.IntN(simLatencySeconds+1)) * time.Second)

		delay := time.Duration(w.rng.IntN(8)) * time.Second

		// A host whose validators are down is one that gets a repair lease;
		// its agent is late more often, so that such leases expire and are
		// taken up again too.
		late := 6
		if a.dead || w.now.Before(a.outageUntil) {
			late = 14
		}

		switch n := w.rng.IntN(20); {
		case w.calm:
		case n == 0:
			delay = time.Hour
		case n < 4:
			delay = time.Duration(10+w.rng.IntN(60)) * time.Second
		case n < late:
			// Start in the last moments before the deadline.
			delay = max(a.deadline.Sub(w.now)-time.Duration(1+w.rng.IntN(25))*time.Second, 0)
		}

		// Now and then the agent's clock is slow by more than the grace the
		// controller gives: it starts a lease that the controller has just
		// expired. The controller takes such a lease up again.
		a.overrun = !w.calm && w.rng.IntN(20) < late-4
		if a.overrun {
			a.deadline = a.deadline.Add(time.Duration(61+w.rng.IntN(90)) * time.Second)
			delay = max(a.deadline.Sub(w.now)-time.Duration(1+w.rng.IntN(40))*time.Second, 0)
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
	case protocol.DirectiveOverride:
		return reject("the simulated agents have no overrides file")
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
