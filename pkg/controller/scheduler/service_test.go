package scheduler

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func quietLog() logrus.FieldLogger {
	l := logrus.New()
	l.SetOutput(io.Discard)

	return l
}

// harness runs a service on a fake store with a fake clock.
type harness struct {
	t      *testing.T
	ctx    context.Context //nolint:containedctx // test fixture, lives as long as the test.
	store  *fakeStore
	svc    *service
	now    time.Time
	ids    int
	health fleet.NetworkHealth
	nextV  uint64
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		t:     t,
		ctx:   t.Context(),
		store: newFakeStore(),
		now:   testGenesis.Add(100*testEpoch + 10*time.Second),
	}
	h.refreshHealth()
	h.restart()

	return h
}

// restart builds a new service on the same store.
func (h *harness) restart() {
	defaults := fleet.DefaultSettings()
	defaults.PolicyReboot = fleet.PolicyAuto

	svc, ok := New(quietLog(), Options{
		Store:        h.store,
		Health:       func() fleet.NetworkHealth { return h.health },
		Liveness:     h.store.listLiveness,
		Attested:     h.store.attestedSlots,
		Defaults:     defaults,
		DeployWindow: 24 * time.Hour,
		Now:          func() time.Time { return h.now },
		NewID: func() string {
			h.ids++

			return fmt.Sprintf("id%03d", h.ids)
		},
	}).(*service)
	require.True(h.t, ok)

	h.svc = svc
}

func (h *harness) refreshHealth() {
	epoch := uint64(h.now.Sub(testGenesis) / testEpoch)
	h.health = fleet.NetworkHealth{
		UpdatedAt: h.now, GenesisTime: testGenesis,
		SecondsPerSlot: testSecondsPerSlot, SlotsPerEpoch: testSlotsPerEpoch,
		Phases:       fleet.SinglePhase(testGenesis, testSecondsPerSlot*time.Second),
		CurrentEpoch: epoch, FinalizedEpoch: epoch - 2, FinalityLagEpochs: 2,
		LivenessKnown: true, LivenessEpoch: epoch - 1,
	}
}

// addHost stores a host that synced now with the given needs.
func (h *harness) addHost(name string, weight uint64, needs ...protocol.Need) *fleet.Host {
	host := &fleet.Host{
		Name: name, FirstSeen: h.now.Add(-30 * 24 * time.Hour), LastSeen: h.now,
		Status: &protocol.SyncRequest{ProtocolVersion: protocol.Version, Verbs: slices.Clone(allVerbs), Needs: needs},
	}

	if weight > 0 {
		host.Validators = []identity.ValidatorRange{{Start: h.nextV, End: h.nextV + weight}}
		h.nextV += weight
	}

	h.store.putHost(host)

	return host
}

// sync stores a new sync of the host; change edits the status.
func (h *harness) sync(name string, change func(st *protocol.SyncRequest)) {
	h.t.Helper()

	host := h.store.host(name)
	require.NotNil(h.t, host)

	st := *host.Status
	if change != nil {
		change(&st)
	}

	host.Status = &st
	host.LastSeen = h.now
	h.store.putHost(host)
}

// obeyHolds is the agent of host name: it applies its unanswered hold and
// release-hold directives to the holds it reports, answers them, and the
// scheduler takes the answer.
func (h *harness) obeyHolds(name string) {
	h.t.Helper()

	pending, err := h.store.ListDirectives(h.ctx, store.DirectiveFilter{Host: name, PendingOnly: true})
	require.NoError(h.t, err)

	h.sync(name, func(st *protocol.SyncRequest) {
		holds := slices.Clone(st.Hold.Holds)

		for _, d := range pending {
			if d.Kind != protocol.DirectiveHold && d.Kind != protocol.DirectiveReleaseHold {
				continue
			}

			holds = slices.DeleteFunc(holds, func(x protocol.HostHold) bool { return x.Holder == d.Holder })
			if d.Kind == protocol.DirectiveHold {
				holds = append(holds, protocol.HostHold{Holder: d.Holder, By: d.By.User, Reason: d.Reason, At: h.now})
			}

			st.DirectiveAcks = append(st.DirectiveAcks, protocol.DirectiveAck{ID: d.ID, Status: protocol.DirectiveAccepted})
		}

		slices.SortFunc(holds, protocol.HoldOrder)
		st.Hold.Holds = holds
	})
	h.tick()
}

// advance moves the clock, syncs every host and publishes full liveness and
// an attestation of every validator host for the newest settled slot.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()

	h.now = h.now.Add(d)
	h.refreshHealth()

	hosts, err := h.store.ListHosts(h.ctx)
	require.NoError(h.t, err)

	clock, _ := h.health.SlotAt(h.now)

	for _, host := range hosts {
		h.sync(host.Name, nil)

		if host.Weight() > 0 {
			h.store.putLiveness(fleet.Liveness{
				Host: host.Name, Epoch: h.health.LivenessEpoch, Live: host.Weight(), Total: host.Weight(),
			})
			h.store.attest(host.Name, clock-2)
		}
	}
}

func (h *harness) tick() {
	h.t.Helper()
	require.NoError(h.t, h.svc.reconcile(h.ctx))
}

func (h *harness) assignment(host string) *Assignment {
	h.t.Helper()

	a, err := h.svc.Assignment(h.ctx, host)
	require.NoError(h.t, err)

	return a
}

func (h *harness) openRollouts() []*fleet.Rollout {
	h.t.Helper()

	rs, err := h.store.ListRollouts(h.ctx, store.RolloutFilter{OpenOnly: true})
	require.NoError(h.t, err)

	return rs
}

func (h *harness) rollout(id string) *fleet.Rollout {
	h.t.Helper()

	r, err := h.store.GetRollout(h.ctx, id)
	require.NoError(h.t, err)

	return r
}

// finish reports the host's lease done, drops the served needs and walks
// the clock until the lease is released.
func (h *harness) finish(name string) {
	h.t.Helper()

	lease := h.assignment(name).Lease
	require.NotNil(h.t, lease)

	h.sync(name, func(st *protocol.SyncRequest) {
		op := &protocol.Operation{LeaseID: lease.ID, Phase: protocol.PhaseDone, Disrupted: true}
		for _, n := range lease.Needs {
			op.Steps = append(op.Steps, protocol.Step{NeedID: n.ID, Status: protocol.StepDone})
		}

		st.Operation = op
		st.Needs = slices.DeleteFunc(slices.Clone(st.Needs), func(n protocol.Need) bool {
			return slices.ContainsFunc(lease.Needs, func(g protocol.Need) bool { return g.ID == n.ID })
		})
	})
	h.tick()
	h.advance(5 * time.Second)
	h.tick()

	// The host keeps syncing: a gap of more than hostStaleAfter would end its
	// run of healthy syncs.
	for left := 2 * testEpoch; left > 0; left -= time.Minute {
		h.advance(min(left, time.Minute))
		h.tick()
	}
}

func TestServiceAssignment(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0, packagesNeed("2.0"))
	h.addHost("idle", 0)

	assert.Nil(t, h.assignment("a").Lease, "nothing before the first reconcile")
	assert.NotNil(t, h.svc.Plan(), "an empty plan before the first reconcile")

	h.tick()

	a := h.assignment("a")
	require.NotNil(t, a.Lease)
	assert.Equal(t, h.now, a.Lease.GrantedAt)
	assert.Equal(t, h.now.Add(30*time.Minute), a.Lease.ExpiresAt)
	assert.Equal(t, int64(1800), a.Lease.TTLSeconds)
	assert.Equal(t, []protocol.Need{packagesNeed("2.0")}, a.Lease.Needs)
	assert.Nil(t, a.Queue)

	b := h.assignment("b")
	assert.Nil(t, b.Lease)
	assert.Equal(t, &protocol.Queue{Position: 1, Reason: "after canary a"}, b.Queue)

	idle := h.assignment("idle")
	assert.Nil(t, idle.Lease)
	assert.Nil(t, idle.Queue)
	assert.Empty(t, idle.Directives)

	// TTLSeconds is the time left, and never negative.
	h.now = h.now.Add(100 * time.Second)
	assert.Equal(t, int64(1700), h.assignment("a").Lease.TTLSeconds)

	h.now = h.now.Add(time.Hour)
	assert.Equal(t, int64(0), h.assignment("a").Lease.TTLSeconds)

	// A lease is restated while the controller verifies.
	h.now = h.now.Add(-time.Hour)
	h.sync("a", func(st *protocol.SyncRequest) {
		st.Operation = &protocol.Operation{
			LeaseID: a.Lease.ID, Phase: protocol.PhaseDone, Disrupted: true,
			Steps: []protocol.Step{{NeedID: a.Lease.Needs[0].ID, Status: protocol.StepDone}},
		}
		st.Needs = nil
	})
	h.tick()
	require.NotNil(t, h.assignment("a").Lease)
	assert.Equal(t, SlotVerifying, h.svc.Plan().Hosts["a"].Slot)

	// Hold directives are restated until the agent answers.
	_, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"idle"}, Reason: "debugging"})
	require.NoError(t, err)
	h.tick()

	dirs := h.assignment("idle").Directives
	require.Len(t, dirs, 1)
	assert.Equal(t, protocol.DirectiveHold, dirs[0].Kind)
	assert.Equal(t, fleet.HolderStaff, dirs[0].Holder)
	assert.Equal(t, "debugging", dirs[0].Reason)

	h.tick()
	assert.Equal(t, dirs, h.assignment("idle").Directives)

	h.obeyHolds("idle")
	assert.Empty(t, h.assignment("idle").Directives)
}

func TestServiceWriteFailureShowsNothing(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))

	h.store.failWrites = true
	before := h.svc.Plan()

	err := h.svc.reconcile(h.ctx)
	require.ErrorIs(t, err, errWrite)
	assert.Nil(t, h.assignment("a").Lease, "an uncommitted grant is not visible")
	assert.Same(t, before, h.svc.Plan(), "the plan of a failed reconcile is not published")
	assert.Empty(t, h.openRollouts())

	h.store.failWrites = false
	h.tick()
	assert.NotNil(t, h.assignment("a").Lease)
	assert.Len(t, h.openRollouts(), 1)

	// Actions fail as a whole too.
	h.store.failWrites = true
	_, err = h.svc.Reboot(h.ctx, "alice", RebootRequest{All: true})
	require.ErrorIs(t, err, errWrite)
	require.ErrorIs(t, replaceSettings(h.ctx, h.svc, "alice", fleet.DefaultSettings()), errWrite)

	h.store.failWrites = false
	assert.Len(t, h.openRollouts(), 1)
}

func TestServiceRestartChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 10, packagesNeed("2.0"))
	h.addHost("b", 10, packagesNeed("2.0"))
	h.advance(0)
	h.tick()

	lease := h.assignment("a").Lease
	require.NotNil(t, lease)

	dump := h.store.dump()
	plan := h.svc.Plan()

	h.restart()
	assert.Equal(t, lease, h.assignment("a").Lease, "the lease survives the restart")

	h.tick()
	assert.Equal(t, dump, h.store.dump(), "a rebuilt scheduler decides nothing new")
	assert.Equal(t, plan, h.svc.Plan())
}

// TestServiceRestartAfterAnOutage: a controller that was down for longer
// than leaseTtl and hostStaleAfter starts again. The agents could not sync
// meanwhile; their silence counts from the start.
func TestServiceRestartAfterAnOutage(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0, packagesNeed("2.0"))
	h.tick()

	lease := h.assignment("a").Lease
	require.NotNil(t, lease)

	h.sync("a", func(st *protocol.SyncRequest) {
		st.Operation = &protocol.Operation{
			LeaseID: lease.ID, Phase: protocol.PhaseApplying, Disrupted: true,
			Steps: []protocol.Step{{NeedID: lease.Needs[0].ID, Status: protocol.StepRunning}},
		}
	})
	h.tick()

	state := func() fleet.LeaseState {
		l, err := h.store.GetLease(h.ctx, lease.ID)
		require.NoError(t, err)

		return l.State
	}

	require.Equal(t, fleet.LeaseRunning, state())

	ttl := fleet.DefaultSettings().LeaseTTL.Std()

	// Down for three times leaseTtl: no sync, no reconcile.
	h.now = h.now.Add(3 * ttl)
	h.refreshHealth()
	h.restart()
	h.tick()

	assert.Equal(t, fleet.LeaseRunning, state(), "the outage is not the agent's silence")
	assert.Equal(t, fleet.RolloutActive, h.openRollouts()[0].State)
	assert.Equal(t, SlotLeased, h.svc.Plan().Hosts["a"].Slot)
	assert.Equal(t, SlotQueued, h.svc.Plan().Hosts["b"].Slot, "not stale either")

	// Manual actions judge silence the same way.
	res, err := h.svc.Reboot(h.ctx, "alice", RebootRequest{Hosts: []string{"b"}, Staggered: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, res.Hosts)

	// "b" syncs again, "a" stays silent for leaseTtl after the start.
	h.now = h.now.Add(ttl)
	h.refreshHealth()
	h.sync("b", nil)
	h.tick()
	assert.Equal(t, fleet.LeaseRunning, state())

	h.now = h.now.Add(time.Second)
	h.refreshHealth()
	h.sync("b", nil)
	h.tick()
	assert.Equal(t, fleet.LeaseFailed, state())
	assert.Equal(t, SlotQueued, h.svc.Plan().Hosts["b"].Slot)
}

func TestServiceStartStop(t *testing.T) {
	h := newHarness(t)
	h.store.putHost(&fleet.Host{
		Name: "a", LastSeen: time.Now(),
		Status: &protocol.SyncRequest{ProtocolVersion: protocol.Version, Verbs: allVerbs, Needs: []protocol.Need{packagesNeed("2.0")}},
	})

	svc := New(quietLog(), Options{
		Store:    h.store,
		Liveness: h.store.listLiveness,
		Health: func() fleet.NetworkHealth {
			return fleet.NetworkHealth{
				UpdatedAt: time.Now(), GenesisTime: testGenesis,
				SecondsPerSlot: testSecondsPerSlot, SlotsPerEpoch: testSlotsPerEpoch,
				Phases: fleet.SinglePhase(testGenesis, testSecondsPerSlot*time.Second),
			}
		},
		Defaults:     fleet.DefaultSettings(),
		TickInterval: time.Hour, // only the first pass and nudges reconcile
	})

	require.NoError(t, svc.Start(t.Context()))
	require.Error(t, svc.Start(t.Context()), "already started")

	require.Eventually(t, func() bool {
		a, err := svc.Assignment(t.Context(), "a")

		return err == nil && a.Lease != nil
	}, 5*time.Second, 5*time.Millisecond, "the first pass runs at once")

	h.store.putHost(&fleet.Host{
		Name: "b", LastSeen: time.Now(),
		Status: &protocol.SyncRequest{ProtocolVersion: protocol.Version, Verbs: allVerbs, Needs: []protocol.Need{rebootNeed()}},
	})
	svc.Nudge()
	svc.Nudge()

	require.Eventually(t, func() bool {
		rs, err := h.store.ListRollouts(t.Context(), store.RolloutFilter{OpenOnly: true})

		return err == nil && len(rs) == 2
	}, 5*time.Second, 5*time.Millisecond, "a nudge reconciles without waiting for the tick")

	require.NoError(t, svc.Stop())
	require.NoError(t, svc.Stop())

	t.Run("no store", func(t *testing.T) {
		require.Error(t, New(quietLog(), Options{}).Start(t.Context()))
	})

	t.Run("a cancelled context stops the loop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		svc := New(quietLog(), Options{Store: newFakeStore(), TickInterval: time.Millisecond})

		require.NoError(t, svc.Start(ctx))
		cancel()
		require.NoError(t, svc.Stop())
	})

	t.Run("a failing store does not stop the loop", func(t *testing.T) {
		st := newFakeStore()
		st.putHost(&fleet.Host{
			Name: "a", LastSeen: time.Now(),
			Status: &protocol.SyncRequest{ProtocolVersion: protocol.Version, Verbs: allVerbs, Needs: []protocol.Need{packagesNeed("2.0")}},
		})
		st.failNext = 3

		svc := New(quietLog(), Options{Store: st, TickInterval: time.Millisecond, Defaults: fleet.DefaultSettings()})
		require.NoError(t, svc.Start(t.Context()))

		require.Eventually(t, func() bool {
			rs, err := st.ListRollouts(t.Context(), store.RolloutFilter{OpenOnly: true})

			return err == nil && len(rs) == 1
		}, 5*time.Second, 5*time.Millisecond)
		require.NoError(t, svc.Stop())
	})
}

// The listener hears of a plan that differs and of every write, and nothing
// of a tick that finds the world as the tick before it left it.
func TestServiceTellsWhenSomethingChanged(t *testing.T) {
	h := newHarness(t)

	changed := 0
	h.svc.opts.Changed = func() { changed++ }

	h.addHost("a", 10)
	h.tick()
	require.Positive(t, changed, "the first plan with the host")

	changed = 0

	h.tick()
	h.tick()
	assert.Zero(t, changed, "an idle tick computes the same plan at another time")
	assert.NotEqual(t, time.Time{}, h.svc.Plan().ComputedAt)

	h.health.HeadSlot++
	h.tick()
	assert.Equal(t, 1, changed, "the plan carries the network's view")

	changed = 0

	_, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a"}, Reason: "test"})
	require.NoError(t, err)
	assert.Positive(t, changed, "an action writes")
}

func TestSettings(t *testing.T) {
	h := newHarness(t)

	got, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.InEpsilon(t, 10.0, got.MaxUnavailablePercent, 0.0001)

	bad := got
	bad.MaxUnavailablePercent = 101
	err = replaceSettings(h.ctx, h.svc, "alice", bad)
	require.ErrorIs(t, err, ErrInvalidAction)
	assert.ErrorContains(t, err, "maxUnavailablePercent")
	assert.Empty(t, h.store.auditActions())

	next := got
	next.MaxUnavailablePercent = 25
	next.PolicyImage = fleet.PolicyManual
	require.NoError(t, replaceSettings(h.ctx, h.svc, "alice", next))

	stored, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, next, stored)

	entry := h.store.lastAudit()
	assert.Equal(t, "settings.update", entry.Action)
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, h.now, entry.Time)
	assert.Equal(t, got, entry.Detail["before"])
	assert.Equal(t, next, entry.Detail["after"])

	// The next reconcile uses them.
	h.addHost("a", 0)
	h.tick()
	assert.Equal(t, 1, h.svc.Plan().Budget.MaxLeasedHosts)
}

func TestUnauditedChangesAreTakenBack(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0)

	before, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)

	// The settings are saved, then the audit entry cannot be written.
	next := before
	next.MaxUnavailablePercent = 77
	h.store.failSkip, h.store.failNext = 1, 1

	require.ErrorIs(t, replaceSettings(h.ctx, h.svc, "alice", next), errWrite)

	got, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, before, got, "a settings change without an audit entry must not stay")

	// A hold and its audit entry are one write.
	h.store.failNext = 1

	_, err = h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a"}, Reason: "debugging"})
	require.ErrorIs(t, err, errWrite)
	h.tick()
	assert.Empty(t, h.assignment("a").Directives, "a hold without an audit entry must not stay")
	assert.Empty(t, h.store.auditActions())
}

// failLease makes the host's agent report its lease failed.
func (h *harness) failLease(name string) {
	h.t.Helper()

	lease := h.assignment(name).Lease
	require.NotNil(h.t, lease)

	h.sync(name, func(st *protocol.SyncRequest) {
		st.Operation = &protocol.Operation{
			LeaseID: lease.ID, Phase: protocol.PhaseFailed, Disrupted: true, Error: "container did not start",
			Steps: []protocol.Step{{NeedID: lease.Needs[0].ID, Status: protocol.StepFailed}},
		}
	})
	h.tick()
}

func TestReservedActorNames(t *testing.T) {
	tests := []struct{ actor, want string }{
		{"", "anonymous"},
		{"alice", "alice"},
		{"scheduler", "user:scheduler"},
		{"schedulers", "schedulers"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, actorName(tt.actor), "actor %q", tt.actor)
	}
}

func TestTickReadsOnlyWhatIsOpen(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0)
	h.tick()
	require.NotNil(t, h.assignment("a").Lease)

	// A manual rollout whose directive "b" answers; it waits for the one
	// slot that "a" holds.
	res, err := h.svc.Reboot(h.ctx, "alice", RebootRequest{Hosts: []string{"b"}, Staggered: true})
	require.NoError(t, err)

	dir := h.assignment("b").Directives[0]
	need := protocol.Need{
		ID: protocol.ManualNeedID(dir.ID), Kind: protocol.NeedReboot, DirectiveID: dir.ID,
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}

	h.sync("b", func(st *protocol.SyncRequest) {
		st.Needs = []protocol.Need{need}
		st.DirectiveAcks = []protocol.DirectiveAck{{ID: dir.ID, Status: protocol.DirectiveAccepted}}
	})
	h.tick()
	h.sync("b", func(st *protocol.SyncRequest) { st.DirectiveAcks = nil })
	require.Empty(t, h.assignment("b").Directives, "the directive is answered")

	// The pending directives, and those of the open manual rollout.
	steady := []string{"rollouts:open", "directives:pending", "directives:rollouts"}
	h.store.reads = nil

	for range 5 {
		h.advance(2 * time.Second)
		h.tick()
	}

	assert.Equal(t, steady, h.store.readKinds(), "a tick reads no closed rollout and no other answered directive")
	assert.Equal(t, fleet.HostPending, h.rollout(res.Rollout).Host("b").State,
		"the answered directive of the open manual rollout is still known")
	assert.Equal(t, "hosts at once: 1 of 1 busy", h.assignment("b").Queue.Reason)

	// A restarted scheduler reads the same and decides the same.
	h.restart()
	h.store.reads = nil
	h.advance(2 * time.Second)
	h.tick()
	assert.Equal(t, steady, h.store.readKinds())
	assert.Equal(t, fleet.HostPending, h.rollout(res.Rollout).Host("b").State)

	h.finish("a")
	require.NotNil(t, h.assignment("b").Lease)
	assert.Equal(t, []protocol.Need{need}, h.assignment("b").Lease.Needs)

	h.finish("b")
	assert.Equal(t, fleet.RolloutCompleted, h.rollout(res.Rollout).State)

	h.store.reads = nil
	h.advance(2 * time.Second)
	h.tick()
	assert.Equal(t, []string{"rollouts:open", "directives:pending"}, h.store.readKinds())
}

// reports makes the hosts report exactly the packages need of a version.
func (h *harness) reports(version string, hosts ...string) {
	h.t.Helper()

	for _, name := range hosts {
		h.sync(name, func(st *protocol.SyncRequest) { st.Needs = []protocol.Need{packagesNeed(version)} })
	}
}

// running makes the host's agent report its lease as being worked on.
func (h *harness) running(name string) string {
	h.t.Helper()

	lease := h.assignment(name).Lease
	require.NotNil(h.t, lease, "host %s holds no lease", name)

	h.sync(name, func(st *protocol.SyncRequest) {
		op := &protocol.Operation{LeaseID: lease.ID, Phase: protocol.PhaseApplying, Disrupted: true}
		for _, n := range lease.Needs {
			op.Steps = append(op.Steps, protocol.Step{NeedID: n.ID, Status: protocol.StepRunning})
		}

		st.Operation = op
	})

	return lease.ID
}

// leased names the hosts that hold a committed lease.
func (h *harness) leased() []string {
	h.t.Helper()

	open, err := h.store.ListLeases(h.ctx, store.LeaseFilter{OpenOnly: true})
	require.NoError(h.t, err)

	hosts := make([]string, 0, len(open))
	for _, l := range open {
		hosts = append(hosts, l.Host)
	}

	slices.Sort(hosts)

	return hosts
}

// openRollout returns the open rollout with the key.
func (h *harness) openRollout(key string) *fleet.Rollout {
	h.t.Helper()

	for _, r := range h.openRollouts() {
		if r.Key == key {
			return r
		}
	}

	h.t.Fatalf("no open rollout %s", key)

	return nil
}

// replay drives a harness. In restart mode the service is rebuilt from the
// store (with the start time of the one before it) ahead of every reconcile
// and every action, and the store is dumped after each: a scheduler that
// restarts at any point must write what one that keeps running writes.
type replay struct {
	*harness
	restart bool
	dumps   []string
}

func newReplay(t *testing.T, restart bool) *replay {
	t.Helper()

	return &replay{harness: newHarness(t), restart: restart}
}

func (r *replay) fresh() {
	if r.restart {
		since := r.svc.since
		r.harness.restart()
		r.svc.since = since
	}
}

func (r *replay) dump() { r.dumps = append(r.dumps, r.store.dump()) }

// pass reconciles once.
func (r *replay) pass() {
	r.t.Helper()
	r.fresh()
	r.tick()
	r.dump()
}

func (r *replay) skip(id string, hosts ...string) {
	r.t.Helper()
	r.fresh()

	_, err := r.svc.Skip(r.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: hosts, Reason: "broken"})
	require.NoError(r.t, err)
	r.dump()
}

// done reports the host's lease done and walks until it is released.
func (r *replay) done(host string) {
	r.t.Helper()

	lease := r.assignment(host).Lease
	require.NotNil(r.t, lease, "no lease on %s", host)

	r.sync(host, func(st *protocol.SyncRequest) {
		op := &protocol.Operation{LeaseID: lease.ID, Phase: protocol.PhaseDone, Disrupted: true}
		for _, n := range lease.Needs {
			op.Steps = append(op.Steps, protocol.Step{NeedID: n.ID, Status: protocol.StepDone})
		}

		st.Operation = op
		st.Needs = slices.DeleteFunc(slices.Clone(st.Needs), func(n protocol.Need) bool {
			return slices.ContainsFunc(lease.Needs, func(g protocol.Need) bool { return g.ID == n.ID })
		})
	})
	r.pass()
	r.advance(5 * time.Second)
	r.pass()

	for left := 2 * testEpoch; left > 0; left -= time.Minute {
		r.advance(min(left, time.Minute))
		r.pass()
	}

	require.NotContains(r.t, r.leased(), host, "lease of %s not released", host)
}

// endLeaseID ends a lease by its ID as a person does.
func (r *replay) endLeaseID(actor, lease string) (*ActionResult, error) {
	r.t.Helper()
	r.fresh()

	res, err := r.svc.EndLease(r.ctx, actor, EndLeaseRequest{Lease: lease, Reason: "taken away"})
	r.dump()

	return res, err
}

// storedLease reads a lease from the store.
func (r *replay) storedLease(id string) *fleet.Lease {
	r.t.Helper()

	l, err := r.store.GetLease(r.ctx, id)
	require.NoError(r.t, err)

	return l
}

// reportOp makes the host's agent report an operation for the lease; the
// statuses are those of the lease's needs, in order.
func (r *replay) reportOp(
	host, lease string, phase protocol.Phase, disrupted bool, statuses ...protocol.StepStatus,
) {
	r.t.Helper()

	l := r.storedLease(lease)
	require.Len(r.t, statuses, len(l.Needs))

	op := &protocol.Operation{LeaseID: lease, Phase: phase, Disrupted: disrupted}
	if phase == protocol.PhaseFailed {
		op.Error = "container did not start"
	}

	for i, n := range l.Needs {
		op.Steps = append(op.Steps, protocol.Step{NeedID: n.ID, Status: statuses[i]})
	}

	r.sync(host, func(st *protocol.SyncRequest) { st.Operation = op })
}

// Found by the simulation: a lease that a person ended before the controller
// knew of an operation is withheld from the agent. An agent that has started
// it all the same reports the operation, and when it reports it as over while
// the scheduler has not taken the report in yet (a reconcile that failed, or
// has not run), a response without the lease made it forget the operation:
// the lease then expired as never started although the host had been down.
// The lease is restated, with the mark, to an agent that reports an operation
// for it.
func TestAgentThatReportsAnEndedLeaseIsToldOfIt(t *testing.T) {
	r := newReplay(t, false)

	for _, name := range []string{"a", "b", "c"} {
		r.addHost(name, 0)
	}

	r.reports("2.0", "a", "b", "c")
	r.pass()

	lease := r.assignment("a").Lease.ID

	_, err := r.endLeaseID("alice", lease)
	require.NoError(t, err)
	require.Nil(t, r.assignment("a").Lease)

	// The agent had started. No reconcile has seen its report yet.
	r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone)
	require.Equal(t, fleet.LeaseGranted, r.storedLease(lease).State)

	offered := r.assignment("a").Lease
	require.NotNil(t, offered, "the agent keeps its operation until the scheduler has taken it in")
	assert.Equal(t, lease, offered.ID)
	assert.True(t, offered.Cancel)

	r.pass()
	assert.Equal(t, fleet.LeaseCancelled, r.storedLease(lease).State)
}

// set changes the stored settings, as the settings page does, without an
// audit entry of its own.
func (r *replay) set(change func(s *fleet.Settings)) {
	r.t.Helper()

	s, err := r.svc.Settings(r.ctx)
	require.NoError(r.t, err)

	change(&s)
	require.NoError(r.t, s.Validate())
	require.NoError(r.t, r.store.SaveSettings(r.ctx, s, s))
}

// resume resumes a rollout as a person does.
func (r *replay) resume(id string) *ActionResult {
	r.t.Helper()
	r.fresh()

	res, err := r.svc.Resume(r.ctx, "alice", ScopeRequest{Rollout: id, Reason: "looks good"})
	require.NoError(r.t, err)
	r.dump()

	return res
}

// audited counts the audit entries with the action.
func (h *harness) audited(action string) int {
	return len(slices.DeleteFunc(h.store.auditActions(), func(a string) bool { return a != action }))
}

// TestPatchSettings: a patch is applied to the settings as they are under the
// lock, validated, stored and audited with before and after; a patch that
// changes nothing writes nothing; a failing patch changes nothing.
func TestPatchSettings(t *testing.T) {
	h := newHarness(t)

	base, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)

	audits := func() int { return len(h.store.auditActions()) }

	// Nothing named: no write, no entry.
	got, changed, err := h.svc.PatchSettings(h.ctx, "alice", func(*fleet.Settings) error { return nil })
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, base, got)
	assert.Zero(t, audits())

	// What is set already: the same.
	got, changed, err = h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
		s.MinPeers = base.MinPeers
		s.Paused = base.Paused

		return nil
	})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, base, got)
	assert.Zero(t, audits())

	// A change, by a request with an authorization.
	ctx := WithAuthorization(h.ctx, Authorization{By: "admin", Teams: []string{"panda"}})

	got, changed, err = h.svc.PatchSettings(ctx, "alice", func(s *fleet.Settings) error {
		s.MinPeers = 7

		return nil
	})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 7, got.MinPeers)

	entry := h.store.lastAudit()
	assert.Equal(t, "settings.update", entry.Action)
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, base, entry.Detail[keyBefore])
	assert.Equal(t, got, entry.Detail[keyAfter])
	assert.Equal(t, "admin", entry.Detail[keyAuthorizedBy])
	assert.Equal(t, 1, audits())

	// An error of the patch is the caller's and changes nothing, also when
	// the patch wrote into what it was given.
	_, changed, err = h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
		s.MinPeers = 9

		return assert.AnError
	})
	require.ErrorIs(t, err, assert.AnError)
	assert.False(t, changed)

	// A result that is not valid is refused.
	_, changed, err = h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
		s.MinPeers = 0

		return nil
	})
	require.ErrorIs(t, err, ErrInvalidAction)
	assert.False(t, changed)

	now, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, got, now)
	assert.Equal(t, 1, audits())

	// A patch sees what an action wrote before it: the global pause is not
	// overwritten by a patch that does not name it, with a restart between.
	_, err = h.svc.Pause(h.ctx, "bob", ScopeRequest{})
	require.NoError(t, err)

	h.restart()

	got, _, err = h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
		s.ReadyPeerShare = 0.25

		return nil
	})
	require.NoError(t, err)
	assert.True(t, got.Paused)
	assert.Equal(t, 7, got.MinPeers)
	assert.InDelta(t, 0.25, got.ReadyPeerShare, 0)
}

// TestPatchSettingsOverlapping: patches that run at once each change what
// they name; none writes back what it read before another one wrote.
func TestPatchSettingsOverlapping(t *testing.T) {
	h := newHarness(t)

	const rounds = 50

	var wg sync.WaitGroup

	patch := func(apply func(i int, s *fleet.Settings)) {
		defer wg.Done()

		for i := range rounds {
			_, _, err := h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
				apply(i, s)

				return nil
			})
			assert.NoError(t, err)
		}
	}

	wg.Add(3)

	go patch(func(i int, s *fleet.Settings) { s.MinPeers = 4 + i })
	go patch(func(i int, s *fleet.Settings) { s.ReadyAttestations = uint64(i%30) + 1 })
	go patch(func(i int, s *fleet.Settings) { s.PauseAfterCanaryImage = i%2 == 1 })

	wg.Wait()

	got, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, 4+rounds-1, got.MinPeers)
	assert.Equal(t, uint64((rounds-1)%30)+1, got.ReadyAttestations)
	assert.True(t, got.PauseAfterCanaryImage)
}

// BenchmarkTick is one tick of the service against the SQLite store with a
// fleet of 1000 hosts in the middle of a rollout: what is read (the hosts,
// their liveness, what is open), the reconcile, and the write of what
// changed. BenchmarkSync is what one agent's sync costs the store and the
// scheduler next to it.
func BenchmarkTick(b *testing.B) {
	svc, _, _ := storedFleet(b, 1000)

	for b.Loop() {
		if err := svc.reconcile(b.Context()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSync(b *testing.B) {
	svc, st, sync := storedFleet(b, 1000)
	i := 0

	for b.Loop() {
		in := sync(i % 1000)
		i++

		if _, err := st.RecordSync(b.Context(), in); err != nil {
			b.Fatal(err)
		}

		if _, err := svc.Assignment(b.Context(), in.Cert.Subject); err != nil {
			b.Fatal(err)
		}
	}
}

// storedFleet is a service on the in-memory SQLite store with n hosts of 32
// validators that all report the same packages need, ticked until leases
// are open. sync builds the sync of the i-th host.
func storedFleet(b *testing.B, n int) (svc *service, st store.Store, sync func(i int) *store.HostSync) {
	b.Helper()

	ctx := b.Context()
	now := testGenesis.Add(100*testEpoch + 10*time.Second)
	epoch := uint64(now.Sub(testGenesis) / testEpoch)

	st = store.NewMemory(quietLog())
	if err := st.Start(ctx); err != nil {
		b.Fatal(err)
	}

	b.Cleanup(func() { _ = st.Stop() })

	ranges := &fleet.ValidatorRanges{Ranges: make([]fleet.HostRange, 0, n)}
	for i := range n {
		ranges.Ranges = append(ranges.Ranges, fleet.HostRange{
			Host:           fmt.Sprintf("host-%05d", i),
			ValidatorRange: identity.ValidatorRange{Start: uint64(i) * 32, End: uint64(i+1) * 32},
		})
	}

	if err := st.SetValidatorRanges(ctx, ranges); err != nil {
		b.Fatal(err)
	}

	sync = func(i int) *store.HostSync {
		name := fmt.Sprintf("host-%05d", i)

		return &store.HostSync{
			Cert: &identity.Certificate{Claims: identity.Claims{Subject: name}, Hash: "hash-" + name},
			At:   now,
			Request: &protocol.SyncRequest{
				Verbs: allVerbs, Needs: []protocol.Need{packagesNeed("2.0")},
				Node: protocol.NodeHealth{
					EL: &protocol.ELHealth{Reachable: true, Peers: 20},
					CL: &protocol.CLHealth{Reachable: true, Peers: 20},
				},
			},
		}
	}

	rows := make([]fleet.Liveness, 0, n)

	for i := range n {
		in := sync(i)
		if _, err := st.RecordSync(ctx, in); err != nil {
			b.Fatal(err)
		}

		rows = append(rows, fleet.Liveness{Host: in.Cert.Subject, Epoch: epoch - 1, Live: 32, Total: 32})
	}

	ids := 0
	svc, _ = New(quietLog(), Options{
		Store:    st,
		Liveness: func(int) []fleet.Liveness { return rows },
		Health: func() fleet.NetworkHealth {
			return fleet.NetworkHealth{
				UpdatedAt: now, GenesisTime: testGenesis, SlotsPerEpoch: testSlotsPerEpoch,
				Phases:       fleet.SinglePhase(testGenesis, testSecondsPerSlot*time.Second),
				CurrentEpoch: epoch, FinalizedEpoch: epoch - 2, FinalityLagEpochs: 2,
				LivenessKnown: true, LivenessEpoch: epoch - 1,
			}
		},
		Defaults: fleet.DefaultSettings(),
		Now:      func() time.Time { return now },
		NewID: func() string {
			ids++

			return fmt.Sprintf("id%06d", ids)
		},
	}).(*service)

	if err := svc.reconcile(ctx); err != nil {
		b.Fatal(err)
	}

	return svc, st, sync
}
