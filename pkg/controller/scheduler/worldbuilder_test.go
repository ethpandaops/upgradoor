package scheduler

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

var testGenesis = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	testSecondsPerSlot = 12
	testSlotsPerEpoch  = 32
	testEpoch          = testSecondsPerSlot * testSlotsPerEpoch * time.Second

	imgLighthouse = "sigp/lighthouse:unstable"
	imgGeth       = "ethereum/client-go:latest"
	digestOld     = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	digestA       = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB       = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC       = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

var allVerbs = []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot}

// worldBuilder builds small hand-made worlds and carries the outcome of
// each reconcile into the next one, like the store would.
type worldBuilder struct {
	t             *testing.T
	w             *World
	ids           int
	nextValidator uint64
	// autoLive adds a fully live row in the newest complete epoch for every
	// validator host that has none.
	autoLive bool
	// liveOverride is what autoLive writes for a host instead of its weight.
	liveOverride map[string]uint64
	// healthMod changes the beacon view after every refresh.
	healthMod func(n *fleet.NetworkHealth)
	// ended are the leases that ended, in the order in which they did;
	// closedRollouts the rollouts that closed, newest first.
	ended          []*fleet.Lease
	closedRollouts []*fleet.Rollout
}

func newWorldBuilder(t *testing.T) *worldBuilder {
	t.Helper()

	b := &worldBuilder{t: t, autoLive: true, liveOverride: map[string]uint64{}}
	settings := fleet.DefaultSettings()
	settings.PolicyReboot = fleet.PolicyAuto

	b.w = &World{
		Now:      testGenesis.Add(100*testEpoch + 10*time.Second),
		Settings: settings,
		NewID: func() string {
			b.ids++

			return fmt.Sprintf("id%03d", b.ids)
		},
	}
	b.refreshHealth()

	return b
}

// refreshHealth makes the beacon view healthy and current.
func (b *worldBuilder) refreshHealth() {
	epoch := uint64(b.w.Now.Sub(testGenesis) / testEpoch)
	b.w.Health = fleet.NetworkHealth{
		UpdatedAt:         b.w.Now,
		GenesisTime:       testGenesis,
		SecondsPerSlot:    testSecondsPerSlot,
		Phases:            fleet.SinglePhase(testGenesis, testSecondsPerSlot*time.Second),
		SlotsPerEpoch:     testSlotsPerEpoch,
		CurrentEpoch:      epoch,
		FinalizedEpoch:    epoch - 2,
		FinalityLagEpochs: 2,
		LivenessKnown:     true,
		LivenessEpoch:     epoch - 1,
	}

	if b.healthMod != nil {
		b.healthMod(&b.w.Health)
	}
}

// host adds a host with weight validators that synced just now.
func (b *worldBuilder) host(name string, weight uint64, roles ...string) *fleet.Host {
	h := &fleet.Host{
		Name:      name,
		Roles:     roles,
		FirstSeen: b.w.Now.Add(-30 * 24 * time.Hour),
		LastSeen:  b.w.Now,
		Status:    &protocol.SyncRequest{Verbs: slices.Clone(allVerbs)},
	}

	if weight > 0 {
		h.Validators = &identity.ValidatorRange{Start: b.nextValidator, End: b.nextValidator + weight}
		b.nextValidator += weight
	}

	b.w.Hosts = append(b.w.Hosts, h)

	return h
}

func (b *worldBuilder) get(name string) *fleet.Host {
	for _, h := range b.w.Hosts {
		if h.Name == name {
			return h
		}
	}

	b.t.Fatalf("no host %s", name)

	return nil
}

// live sets the host's liveness row in an epoch.
func (b *worldBuilder) live(epoch uint64, name string, live uint64) {
	h := b.get(name)
	b.w.Liveness = slices.DeleteFunc(b.w.Liveness, func(r fleet.Liveness) bool {
		return r.Host == name && r.Epoch == epoch
	})
	b.w.Liveness = append(b.w.Liveness, fleet.Liveness{
		Host: name, Epoch: epoch, Live: live, Total: h.Weight(),
	})
}

// down makes the host established as offline: none of its validators
// attested in the two newest complete epochs, nor will they in the ones that
// autoLive fills in.
func (b *worldBuilder) down(name string) {
	b.liveOverride[name] = 0
	b.live(b.w.Health.LivenessEpoch, name, 0)
	b.live(b.w.Health.LivenessEpoch-1, name, 0)
}

// advance moves the clock; every host that is not in silent syncs at the new
// time, and has kept syncing with the same status in between: a run of
// healthy syncs that the scheduler has on record for its lease has no gap. (A
// host in silent sends nothing, and its run has the gap.)
func (b *worldBuilder) advance(d time.Duration, silent ...string) {
	b.w.Now = b.w.Now.Add(d)

	for _, h := range b.w.Hosts {
		if !slices.Contains(silent, h.Name) {
			h.LastSeen = b.w.Now
		}
	}

	for _, l := range b.w.Leases {
		if l.SyncedUntil != nil && !slices.Contains(silent, l.Host) {
			until := b.w.Now
			l.SyncedUntil = &until
		}
	}

	b.refreshHealth()
}

// step reconciles once and applies the outcome to the world.
func (b *worldBuilder) step() *Outcome {
	b.t.Helper()
	b.fillLiveness()

	out := Reconcile(b.w)
	applyOutcome(b.w, out)

	for _, l := range out.Leases {
		if l.State.Terminal() {
			b.ended = append(b.ended, l)
		}
	}

	for _, r := range out.Rollouts {
		if r.State.Terminal() {
			b.closedRollouts = append([]*fleet.Rollout{r}, b.closedRollouts...)
		}
	}

	return out
}

// fillLiveness adds the rows of the newest complete epoch that are missing.
func (b *worldBuilder) fillLiveness() {
	if b.autoLive && b.w.Health.LivenessKnown {
		epoch := b.w.Health.LivenessEpoch

		for _, h := range b.w.Hosts {
			if h.Weight() == 0 || slices.ContainsFunc(b.w.Liveness, func(r fleet.Liveness) bool {
				return r.Host == h.Name && r.Epoch == epoch
			}) {
				continue
			}

			live := h.Weight()
			if n, ok := b.liveOverride[h.Name]; ok {
				live = n
			}

			b.live(epoch, h.Name, live)
		}
	}
}

// applyOutcome writes an outcome back into a world.
func applyOutcome(w *World, out *Outcome) {
	for _, r := range out.Rollouts {
		w.Rollouts = slices.DeleteFunc(w.Rollouts, func(x *fleet.Rollout) bool { return x.ID == r.ID })

		if !r.State.Terminal() {
			w.Rollouts = append(w.Rollouts, r)
		}
	}

	for _, l := range out.Leases {
		w.Leases = slices.DeleteFunc(w.Leases, func(x *fleet.Lease) bool { return x.ID == l.ID })

		if !l.State.Terminal() {
			w.Leases = append(w.Leases, l)
		}
	}

	for _, d := range out.Directives {
		w.Directives = slices.DeleteFunc(w.Directives, func(x *fleet.Directive) bool { return x.ID == d.ID })
		w.Directives = append(w.Directives, d)
	}

	// As the store does: the broken marks of a host are replaced. The host is
	// replaced, not changed: the pass may still read the old one.
	for _, marks := range out.Broken {
		for i, h := range w.Hosts {
			if h.Name == marks.Host {
				c := *h
				c.Broken = marks.Marks
				w.Hosts[i] = &c
			}
		}
	}
}

// rollout returns the open rollout with the key.
func (b *worldBuilder) rollout(key string) *fleet.Rollout {
	b.t.Helper()

	for _, r := range b.w.Rollouts {
		if r.Key == key {
			return r
		}
	}

	return nil
}

// mustRollout is rollout that fails the test when there is none.
func (b *worldBuilder) mustRollout(key string) *fleet.Rollout {
	b.t.Helper()

	r := b.rollout(key)
	require.NotNil(b.t, r, "no open rollout %s", key)

	return r
}

// closed returns the newest closed rollout with the key.
func (b *worldBuilder) closed(key string) *fleet.Rollout {
	for _, r := range b.closedRollouts {
		if r.Key == key {
			return r
		}
	}

	return nil
}

// lease returns the host's open lease.
func (b *worldBuilder) lease(host string) *fleet.Lease {
	for _, l := range b.w.Leases {
		if l.Host == host {
			return l
		}
	}

	return nil
}

func (b *worldBuilder) leasedHosts() []string {
	out := make([]string, 0, len(b.w.Leases))
	for _, l := range b.w.Leases {
		out = append(out, l.Host)
	}

	slices.Sort(out)

	return out
}

// report makes the host's agent report an operation for its open lease.
func (b *worldBuilder) report(host string, phase protocol.Phase, disrupted bool, statuses ...protocol.StepStatus) {
	b.t.Helper()

	l := b.lease(host)
	require.NotNil(b.t, l, "host %s holds no lease", host)

	op := &protocol.Operation{LeaseID: l.ID, Phase: phase, StartedAt: b.w.Now, UpdatedAt: b.w.Now, Disrupted: disrupted}

	for i, n := range l.Needs {
		status := protocol.StepRunning

		switch {
		case i < len(statuses):
			status = statuses[i]
		case phase == protocol.PhaseDone:
			status = protocol.StepDone
		case phase == protocol.PhaseFailed:
			status = protocol.StepFailed
		}

		op.Steps = append(op.Steps, protocol.Step{NeedID: n.ID, Status: status})
	}

	if phase == protocol.PhaseFailed {
		op.Error = "container did not start"
	}

	h := b.get(host)
	st := *h.Status
	st.Operation = op
	h.Status = &st
}

// finish makes the agent report done and drop the served needs, as an
// agent whose operation took effect does.
func (b *worldBuilder) finish(host string) {
	b.t.Helper()
	b.report(host, protocol.PhaseDone, true)

	h := b.get(host)
	l := b.lease(host)
	h.Status.Needs = slices.DeleteFunc(slices.Clone(h.Status.Needs), func(n protocol.Need) bool {
		return slices.ContainsFunc(l.Needs, func(g protocol.Need) bool { return g.ID == n.ID })
	})
}

// serve walks the host's lease to released: done, a first sync after it,
// then syncs every minute until two epochs have passed (the validators live,
// the node synced throughout).
func (b *worldBuilder) serve(host string) {
	b.t.Helper()
	b.finish(host)
	b.step()
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)
	b.step()
	require.Nil(b.t, b.lease(host), "lease of %s was not released", host)
}

// lastLease returns the lease of the host that ended last.
func (b *worldBuilder) lastLease(host string) *fleet.Lease {
	b.t.Helper()

	for i := len(b.ended) - 1; i >= 0; i-- {
		if b.ended[i].Host == host {
			return b.ended[i]
		}
	}

	b.t.Fatalf("no lease of %s has ended", host)

	return nil
}

// broke marks the host broken for the group of the need, as a lease that
// failed on it long ago did.
func (b *worldBuilder) broke(host string, need protocol.Need) {
	h := b.get(host)
	marks := maps.Clone(h.Broken)

	if marks == nil {
		marks = make(map[string]string, 1)
	}

	marks[needGroup(&need)] = "broke-" + host
	h.Broken = marks
}

// queue returns the waiting reason per host of a plan.
func queueReasons(p *Plan) map[string]string {
	out := make(map[string]string, len(p.Queue))
	for _, q := range p.Queue {
		out[q.Host] = q.Reason
	}

	return out
}

func imageNeed(container, image, to string) protocol.Need {
	return protocol.Need{
		ID:   protocol.ImageNeedID(container, to),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: container, Image: image, FromDigest: digestOld, ToDigest: to,
		},
	}
}

func packagesNeed(version string) protocol.Need {
	pkgs := []protocol.PackageChange{{Name: "docker-ce", From: "1.0", To: version}}

	return protocol.Need{
		ID:       protocol.PackagesNeedID(pkgs),
		Kind:     protocol.NeedPackages,
		Packages: &protocol.PackagesNeed{Packages: pkgs},
	}
}

func packagesKey(version string) string {
	return prefixPackages + protocol.PackagesKey(
		[]protocol.PackageChange{{Name: "docker-ce", From: "1.0", To: version}})
}

func rebootNeed() protocol.Need {
	return protocol.Need{
		ID:     protocol.RebootNeedID,
		Kind:   protocol.NeedReboot,
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonFlag}},
	}
}

// container is a managed container whose registry check saw registryDigest
// at checked.
func container(name, image, running, registryDigest string, checked time.Time) protocol.Container {
	return protocol.Container{
		Name: name, Image: image, Digest: running, State: "running",
		Managed: true, ConfiguredImage: image,
		Registry: &protocol.RegistryCheck{Digest: registryDigest, CheckedAt: checked, Pulled: true},
	}
}

// wantsImage makes the host run image at digestOld and report a need for
// the digest.
func (b *worldBuilder) wantsImage(host, image, to string) {
	h := b.get(host)
	name := "c-" + image
	h.Status.Containers = append(h.Status.Containers, container(name, image, digestOld, to, b.w.Now))
	h.Status.Needs = append(h.Status.Needs, imageNeed(name, image, to))
}

func (b *worldBuilder) wants(host string, needs ...protocol.Need) {
	h := b.get(host)
	h.Status.Needs = append(h.Status.Needs, needs...)
}

// memberStates returns the member state per host of a rollout.
func memberStates(r *fleet.Rollout) map[string]fleet.HostState {
	out := make(map[string]fleet.HostState, len(r.Hosts))
	for i := range r.Hosts {
		out[r.Hosts[i].Host] = r.Hosts[i].State
	}

	return out
}

func auditActions(out *Outcome) []string {
	actions := make([]string, 0, len(out.Audit))
	for _, e := range out.Audit {
		actions = append(actions, e.Action)
	}

	return actions
}

// replaceSettings stores the settings as given, like a patch that names every
// field.
func replaceSettings(ctx context.Context, svc Service, actor string, settings fleet.Settings) error {
	_, _, err := svc.PatchSettings(ctx, actor, func(s *fleet.Settings) error {
		*s = settings

		return nil
	})

	return err
}

// failedOnDigestA builds a fleet in which the host failing rolled to digest A
// and failed, so that the rollout of digest A is halted with it.
func failedOnDigestA(t *testing.T, failing string, hosts map[string]uint64) *worldBuilder {
	t.Helper()

	b := newWorldBuilder(t)

	for _, name := range sortedKeys(hosts) {
		b.host(name, hosts[name])
		b.wantsImage(name, imgLighthouse, digestA)
		b.get(name).HoldWanted = name != failing
	}

	b.step()
	require.Equal(t, []string{failing}, b.leasedHosts())
	b.report(failing, protocol.PhaseFailed, true)

	for name := range hosts {
		b.get(name).HoldWanted = false
	}

	b.step()
	require.Equal(t, fleet.RolloutHalted, b.mustRollout(imageKey(imgLighthouse, digestA)).State)
	require.Empty(t, b.leasedHosts())

	return b
}

// seesDigestB makes the hosts report the tag at digest B.
func seesDigestB(b *worldBuilder, hosts ...string) {
	for _, name := range hosts {
		h := b.get(name)
		h.Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestB, b.w.Now)}
		h.Status.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digestB)}
		h.Status.Operation = nil
	}
}

// seesDigest makes the hosts report the tag at a digest while they still run
// the old one.
func seesDigest(b *worldBuilder, digest string, hosts ...string) {
	for _, name := range hosts {
		h := b.get(name)
		h.Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digest, b.w.Now)}
		h.Status.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digest)}
		h.Status.Operation = nil
	}
}
