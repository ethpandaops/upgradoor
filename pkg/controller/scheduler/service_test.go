package scheduler

import (
	"context"
	"fmt"
	"io"
	"slices"
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
		Store:    h.store,
		Health:   func() fleet.NetworkHealth { return h.health },
		Defaults: defaults,
		Now:      func() time.Time { return h.now },
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
		CurrentEpoch: epoch, FinalizedEpoch: epoch - 2, FinalityLagEpochs: 2,
		LivenessKnown: true, LivenessEpoch: epoch - 1,
	}
}

// addHost stores a host that synced now with the given needs.
func (h *harness) addHost(name string, weight uint64, needs ...protocol.Need) *fleet.Host {
	host := &fleet.Host{
		Name: name, FirstSeen: h.now, LastSeen: h.now,
		Status: &protocol.SyncRequest{Verbs: slices.Clone(allVerbs), Needs: needs},
	}

	if weight > 0 {
		host.Validators = &identity.ValidatorRange{Start: h.nextV, End: h.nextV + weight}
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

// advance moves the clock, syncs every host and publishes full liveness.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()

	h.now = h.now.Add(d)
	h.refreshHealth()

	hosts, err := h.store.ListHosts(h.ctx)
	require.NoError(h.t, err)

	for _, host := range hosts {
		h.sync(host.Name, nil)

		if host.Weight() > 0 {
			h.store.putLiveness(fleet.Liveness{
				Host: host.Name, Epoch: h.health.LivenessEpoch, Live: host.Weight(), Total: host.Weight(),
			})
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
	h.advance(2 * testEpoch)
	h.tick()
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
	assert.Equal(t, &protocol.Queue{Position: 1, Reason: "host limit 1 of 1"}, b.Queue)

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

	// Hold directives are restated until the agent agrees.
	_, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"idle"}, Reason: "debugging"})
	require.NoError(t, err)
	assert.Empty(t, h.assignment("idle").Directives, "visible only after the reconcile committed it")
	h.tick()

	dirs := h.assignment("idle").Directives
	require.Len(t, dirs, 1)
	assert.Equal(t, protocol.DirectiveHold, dirs[0].Kind)
	assert.Equal(t, "debugging", dirs[0].Reason)

	h.tick()
	assert.Equal(t, dirs, h.assignment("idle").Directives)

	h.sync("idle", func(st *protocol.SyncRequest) { st.Hold.Controller = true })
	h.tick()
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
	require.ErrorIs(t, h.svc.UpdateSettings(h.ctx, "alice", fleet.DefaultSettings()), errWrite)

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

func TestServiceStartStop(t *testing.T) {
	h := newHarness(t)
	h.store.putHost(&fleet.Host{
		Name: "a", LastSeen: time.Now(),
		Status: &protocol.SyncRequest{Verbs: allVerbs, Needs: []protocol.Need{packagesNeed("2.0")}},
	})

	svc := New(quietLog(), Options{
		Store: h.store,
		Health: func() fleet.NetworkHealth {
			return fleet.NetworkHealth{
				UpdatedAt: time.Now(), GenesisTime: testGenesis,
				SecondsPerSlot: testSecondsPerSlot, SlotsPerEpoch: testSlotsPerEpoch,
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
		Status: &protocol.SyncRequest{Verbs: allVerbs, Needs: []protocol.Need{rebootNeed()}},
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
			Status: &protocol.SyncRequest{Verbs: allVerbs, Needs: []protocol.Need{packagesNeed("2.0")}},
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

func TestSettings(t *testing.T) {
	h := newHarness(t)

	got, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.InEpsilon(t, 10.0, got.MaxUnavailablePercent, 0.0001)

	bad := got
	bad.MaxUnavailablePercent = 101
	err = h.svc.UpdateSettings(h.ctx, "alice", bad)
	require.ErrorIs(t, err, ErrInvalidAction)
	assert.ErrorContains(t, err, "maxUnavailablePercent")
	assert.Empty(t, h.store.auditActions())

	next := got
	next.MaxUnavailablePercent = 25
	next.PolicyImage = fleet.PolicyManual
	require.NoError(t, h.svc.UpdateSettings(h.ctx, "alice", next))

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

	require.ErrorIs(t, h.svc.UpdateSettings(h.ctx, "alice", next), errWrite)

	got, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, before, got, "a settings change without an audit entry must not stay")

	// The same for a hold.
	h.store.failSkip, h.store.failNext = 1, 1

	_, err = h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a"}, Reason: "debugging"})
	require.ErrorIs(t, err, errWrite)
	assert.False(t, h.store.host("a").HoldWanted, "a hold without an audit entry must not stay")
	assert.Empty(t, h.store.auditActions())
}

func TestRollAction(t *testing.T) {
	h := newHarness(t)

	lighthouse := container("lighthouse", imgLighthouse, digestA, digestA, h.now)
	geth := container("geth", imgGeth, digestA, digestA, h.now)
	custom := container("debug", "someone/debug:x", digestA, "", h.now)
	custom.Custom = true
	unmanaged := protocol.Container{Name: "node-exporter", Image: "prom/node-exporter"}

	withContainers := func(cs ...protocol.Container) func(st *protocol.SyncRequest) {
		return func(st *protocol.SyncRequest) { st.Containers = cs }
	}

	h.addHost("ok", 10)
	h.sync("ok", withContainers(lighthouse, geth, custom, unmanaged))
	h.addHost("held", 0)
	h.sync("held", func(st *protocol.SyncRequest) {
		st.Containers = []protocol.Container{geth}
		st.Hold.Local = true
	})
	h.addHost("noverb", 0)
	h.sync("noverb", func(st *protocol.SyncRequest) {
		st.Containers = []protocol.Container{geth}
		st.Verbs = []protocol.Verb{protocol.VerbReboot}
	})
	h.addHost("onlycustom", 0)
	h.sync("onlycustom", withContainers(custom, unmanaged))

	silent := h.addHost("silent", 0)
	silent.LastSeen = h.now.Add(-time.Hour)
	h.store.putHost(silent)

	res, err := h.svc.Roll(h.ctx, "alice", RollRequest{All: true, Reason: "new config"})
	require.NoError(t, err)

	assert.Equal(t, []string{"ok"}, res.Hosts)
	assert.Equal(t, map[string]string{
		"held":       "held",
		"noverb":     "verb roll not allowed by the agent",
		"onlycustom": "no managed, non-custom container to roll",
		"silent":     "agent silent",
	}, res.Rejected)

	r := h.rollout(res.Rollout)
	assert.Equal(t, fleet.RolloutManual, r.Kind)
	assert.Equal(t, "manual:"+r.ID, r.Key)
	assert.Equal(t, "manual roll of 1 host(s), now: new config", r.Title)
	assert.Equal(t, res.Message, r.Title)
	assert.True(t, r.Approved, "manual rollouts are created approved")
	assert.True(t, r.Force, "not staggered means now")
	assert.False(t, r.IgnoreBudget)
	assert.Equal(t, "alice", r.CreatedBy)
	assert.Equal(t, map[string]fleet.HostState{"ok": fleet.HostExpected}, memberStates(r))

	// One roll directive per managed, non-custom container.
	dirs := h.assignment("ok").Directives
	require.Len(t, dirs, 2)
	assert.Equal(t, protocol.DirectiveRoll, dirs[0].Kind)
	assert.ElementsMatch(t, []string{"lighthouse", "geth"}, []string{dirs[0].Container, dirs[1].Container})
	assert.Equal(t, "new config", dirs[0].Reason)

	entry := h.store.lastAudit()
	assert.Equal(t, "action.roll", entry.Action)
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, r.ID, entry.Target)
	assert.Equal(t, []string{"ok"}, entry.Detail["hosts"])
	assert.Equal(t, false, entry.Detail["ignoreBudget"])

	// The agent answers both directives with a need each; one lease serves
	// both, and it does not wait for readiness.
	h.sync("ok", func(st *protocol.SyncRequest) {
		for _, d := range dirs {
			st.DirectiveAcks = append(st.DirectiveAcks, protocol.DirectiveAck{ID: d.ID, Status: protocol.DirectiveAccepted})
			need := imageNeed(d.Container, imgGeth, digestA)
			need.ID = protocol.ManualNeedID(d.ID)
			need.DirectiveID = d.ID
			st.Needs = append(st.Needs, need)
		}
	})
	h.advance(0)
	h.tick()

	lease := h.assignment("ok").Lease
	require.NotNil(t, lease)
	assert.Len(t, lease.Needs, 2)
	assert.Empty(t, h.assignment("ok").Directives)

	t.Run("selected containers, staggered, ignoreBudget", func(t *testing.T) {
		res, err := h.svc.Roll(h.ctx, "", RollRequest{
			Hosts: []string{"ok", "ok"}, Containers: []string{"geth", "debug"},
			Staggered: true, IgnoreBudget: true,
		})
		require.NoError(t, err)

		r := h.rollout(res.Rollout)
		assert.False(t, r.Force)
		assert.True(t, r.IgnoreBudget)
		assert.Equal(t, "anonymous", r.CreatedBy)
		assert.Equal(t, "manual roll of 1 host(s), staggered", r.Title)
		assert.Len(t, r.Hosts, 1)

		entry := h.store.lastAudit()
		assert.Equal(t, "anonymous", entry.Actor)
		assert.Equal(t, true, entry.Detail["ignoreBudget"])

		all, err := h.store.ListDirectives(h.ctx, store.DirectiveFilter{Host: "ok", PendingOnly: true})
		require.NoError(t, err)
		require.Len(t, all, 1, "the custom container is not rolled")
		assert.Equal(t, "geth", all[0].Container)
		assert.Equal(t, r.ID, all[0].RolloutID)
	})

	t.Run("staggered with force", func(t *testing.T) {
		res, err := h.svc.Roll(h.ctx, "alice", RollRequest{Hosts: []string{"ok"}, Staggered: true, Force: true})
		require.NoError(t, err)
		assert.True(t, h.rollout(res.Rollout).Force)
	})

	t.Run("errors", func(t *testing.T) {
		_, err := h.svc.Roll(h.ctx, "alice", RollRequest{})
		require.ErrorIs(t, err, ErrInvalidAction)

		_, err = h.svc.Roll(h.ctx, "alice", RollRequest{Hosts: []string{"ok", "nope"}})
		require.ErrorIs(t, err, ErrUnknownHost)

		res, err := h.svc.Roll(h.ctx, "alice", RollRequest{Hosts: []string{"held"}})
		require.ErrorIs(t, err, ErrInvalidAction)
		assert.Equal(t, map[string]string{"held": "held"}, res.Rejected)

		res, err = h.svc.Roll(h.ctx, "alice", RollRequest{Hosts: []string{"ok"}, Containers: []string{"nope"}})
		require.ErrorIs(t, err, ErrInvalidAction)
		assert.Contains(t, res.Rejected, "ok")
	})
}

func TestRebootAction(t *testing.T) {
	h := newHarness(t)
	h.addHost("tool", 0)
	h.addHost("v1", 10)
	h.addHost("v2", 10)
	h.addHost("boot", 0)

	boot := h.store.host("boot")
	boot.Roles = []string{fleet.RoleBootnode}
	h.store.putHost(boot)
	h.advance(0)

	current, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)

	current.MaxUnavailablePercent = 100
	require.NoError(t, h.svc.UpdateSettings(h.ctx, "alice", current))

	res, err := h.svc.Reboot(h.ctx, "alice", RebootRequest{All: true, Staggered: true, Reason: "kernel"})
	require.NoError(t, err)
	assert.Equal(t, []string{"boot", "tool", "v1", "v2"}, res.Hosts)
	assert.Empty(t, res.Rejected)
	assert.Equal(t, "action.reboot", h.store.lastAudit().Action)

	r := h.rollout(res.Rollout)
	assert.False(t, r.Force)
	assert.Equal(t, fleet.StageBootnodes, r.Host("boot").Stage)
	assert.Equal(t, fleet.StageNoValidators, r.Host("tool").Stage)
	assert.Equal(t, fleet.StageBatches, r.Host("v1").Stage)

	// Every agent accepts and reports the manual need.
	for _, name := range res.Hosts {
		dirs := h.assignment(name).Directives
		require.Len(t, dirs, 1)
		assert.Equal(t, protocol.DirectiveReboot, dirs[0].Kind)

		h.sync(name, func(st *protocol.SyncRequest) {
			st.DirectiveAcks = []protocol.DirectiveAck{{ID: dirs[0].ID, Status: protocol.DirectiveAccepted}}
			st.Needs = []protocol.Need{{
				ID: protocol.ManualNeedID(dirs[0].ID), Kind: protocol.NeedReboot, DirectiveID: dirs[0].ID,
				Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
			}}
		})
	}

	// The staggered rollout runs through the normal stages.
	order := make([]string, 0, 4)

	for range 4 {
		h.tick()

		leased := ""

		for _, name := range res.Hosts {
			if h.assignment(name).Lease != nil {
				require.Empty(t, leased, "one host at a time here")
				leased = name
			}
		}

		require.NotEmpty(t, leased)
		order = append(order, leased)
		h.finish(leased)
	}

	assert.Equal(t, []string{"tool", "v1", "v2", "boot"}, order)
	assert.Equal(t, fleet.RolloutCompleted, h.rollout(res.Rollout).State)
	assert.Empty(t, h.openRollouts(), "manual needs open no observed-cause rollout")
}

func TestHoldAndUnholdActions(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0)

	_, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a", "nope"}})
	require.ErrorIs(t, err, ErrUnknownHost)
	assert.False(t, h.store.host("a").HoldWanted, "nothing applied")

	_, err = h.svc.Hold(h.ctx, "alice", HoldRequest{})
	require.ErrorIs(t, err, ErrInvalidAction)

	res, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a", "b"}, Reason: "debugging"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, res.Hosts)
	assert.Equal(t, "2 host(s) on hold", res.Message)
	assert.True(t, h.store.host("a").HoldWanted)
	assert.Equal(t, "debugging", h.store.host("a").HoldReason)
	assert.Equal(t, []string{"action.hold", "action.hold"}, h.store.auditActions())

	entry := h.store.lastAudit()
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, "b", entry.Target)
	assert.Equal(t, map[string]any{"holdWanted": false, "holdReason": ""}, entry.Detail["before"])
	assert.Equal(t, map[string]any{"holdWanted": true, "holdReason": "debugging"}, entry.Detail["after"])

	// A held host is never granted, already before the agent agrees.
	h.tick()
	assert.Nil(t, h.assignment("a").Lease)
	assert.Equal(t, "held", h.assignment("a").Queue.Reason)
	assert.Equal(t, SlotHeld, h.svc.Plan().Hosts["b"].Slot)

	res, err = h.svc.Unhold(h.ctx, "bob", HoldRequest{Hosts: []string{"a"}})
	require.NoError(t, err)
	assert.Equal(t, "hold lifted on 1 host(s)", res.Message)
	assert.False(t, h.store.host("a").HoldWanted)
	assert.Empty(t, h.store.host("a").HoldReason)
	assert.Equal(t, "action.unhold", h.store.lastAudit().Action)

	h.tick()
	assert.NotNil(t, h.assignment("a").Lease)
}

func TestPauseAndResumeActions(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, rebootNeed())

	current, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)

	current.PolicyReboot = fleet.PolicyManual
	require.NoError(t, h.svc.UpdateSettings(h.ctx, "alice", current))

	t.Run("global", func(t *testing.T) {
		res, err := h.svc.Pause(h.ctx, "alice", ScopeRequest{Reason: "incident"})
		require.NoError(t, err)
		assert.Equal(t, "paused", res.Message)

		got, err := h.svc.Settings(h.ctx)
		require.NoError(t, err)
		assert.True(t, got.Paused)

		entry := h.store.lastAudit()
		assert.Equal(t, "action.pause", entry.Action)
		assert.Equal(t, "incident", entry.Detail["reason"])

		audits := len(h.store.auditActions())
		_, err = h.svc.Pause(h.ctx, "alice", ScopeRequest{})
		require.NoError(t, err)
		assert.Len(t, h.store.auditActions(), audits, "pausing twice changes and audits nothing")

		h.tick()
		assert.True(t, h.svc.Plan().Paused)
		assert.Equal(t, "paused", h.assignment("a").Queue.Reason)

		_, err = h.svc.Resume(h.ctx, "alice", ScopeRequest{})
		require.NoError(t, err)
		assert.Equal(t, "action.resume", h.store.lastAudit().Action)

		got, err = h.svc.Settings(h.ctx)
		require.NoError(t, err)
		assert.False(t, got.Paused)
	})

	t.Run("rollout", func(t *testing.T) {
		h.tick()

		open := h.openRollouts()
		require.Len(t, open, 1)

		id := open[0].ID
		assert.Equal(t, "waiting for approval (policy manual)", h.assignment("a").Queue.Reason)

		_, err := h.svc.Pause(h.ctx, "alice", ScopeRequest{Rollout: id})
		require.NoError(t, err)
		assert.True(t, h.rollout(id).Paused)
		assert.Equal(t, id, h.store.lastAudit().Target)

		h.tick()
		assert.Equal(t, "rollout paused", h.assignment("a").Queue.Reason)

		// Resume lifts the pause and approves.
		res, err := h.svc.Resume(h.ctx, "bob", ScopeRequest{Rollout: id})
		require.NoError(t, err)
		assert.Equal(t, "rollout approved", res.Message)
		assert.False(t, h.rollout(id).Paused)
		assert.True(t, h.rollout(id).Approved)

		entry := h.store.lastAudit()
		assert.Equal(t, "action.resume", entry.Action)
		assert.Equal(t, "bob", entry.Actor)
		assert.Equal(t, map[string]any{"paused": true, "approved": false}, entry.Detail["before"])
		assert.Equal(t, map[string]any{"paused": false, "approved": true}, entry.Detail["after"])

		h.tick()
		assert.NotNil(t, h.assignment("a").Lease)

		audits := len(h.store.auditActions())
		res, err = h.svc.Resume(h.ctx, "bob", ScopeRequest{Rollout: id})
		require.NoError(t, err)
		assert.Equal(t, "rollout is not paused and needs no approval", res.Message)
		assert.Len(t, h.store.auditActions(), audits)
	})

	t.Run("errors", func(t *testing.T) {
		_, err := h.svc.Pause(h.ctx, "alice", ScopeRequest{Rollout: "nope"})
		require.ErrorIs(t, err, ErrUnknownRollout)

		_, err = h.svc.Resume(h.ctx, "alice", ScopeRequest{Rollout: "nope"})
		require.ErrorIs(t, err, ErrUnknownRollout)

		id := h.openRollouts()[0].ID
		_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: id})
		require.NoError(t, err)

		_, err = h.svc.Pause(h.ctx, "alice", ScopeRequest{Rollout: id})
		require.ErrorIs(t, err, ErrInvalidAction)

		_, err = h.svc.Resume(h.ctx, "alice", ScopeRequest{Rollout: id})
		require.ErrorIs(t, err, ErrInvalidAction)
	})
}

func TestSkipAction(t *testing.T) {
	h := newHarness(t)
	h.addHost("leased", 0, packagesNeed("2.0"))
	h.addHost("waiting", 0, packagesNeed("2.0"))
	h.addHost("other", 0)
	h.tick()

	id := h.openRollouts()[0].ID

	res, err := h.svc.Skip(h.ctx, "alice", RolloutHostsRequest{
		Rollout: id, Hosts: []string{"leased", "waiting", "other"}, Reason: "broken disk",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"waiting"}, res.Hosts)
	assert.Equal(t, map[string]string{
		"leased": "holds a lease for the rollout",
		"other":  "not a member of the rollout",
	}, res.Rejected)

	m := h.rollout(id).Host("waiting")
	assert.Equal(t, fleet.HostSkipped, m.State)
	assert.Equal(t, "skipped: broken disk", m.Error)
	assert.Equal(t, "action.skip", h.store.lastAudit().Action)

	// The skipped host is settled: it keeps reporting the need, is never
	// granted and does not keep the rollout open. TestSkipOutlivesTheRollout
	// follows it past the completion.
	h.finish("leased")
	assert.Nil(t, h.assignment("waiting").Lease)
	assert.Equal(t, fleet.RolloutCompleted, h.rollout(id).State)

	h.advance(2 * time.Second)
	h.tick()
	assert.Empty(t, h.openRollouts())
	assert.Nil(t, h.assignment("waiting").Lease)

	_, err = h.svc.Skip(h.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"waiting"}})
	require.ErrorIs(t, err, ErrInvalidAction, "the rollout is closed")

	_, err = h.svc.Skip(h.ctx, "alice", RolloutHostsRequest{Rollout: "nope", Hosts: []string{"a"}})
	require.ErrorIs(t, err, ErrUnknownRollout)

	t.Run("nothing to skip", func(t *testing.T) {
		h2 := newHarness(t)
		h2.addHost("a", 0, packagesNeed("2.0"))
		h2.tick()

		id := h2.openRollouts()[0].ID

		_, err := h2.svc.Skip(h2.ctx, "alice", RolloutHostsRequest{Rollout: id})
		require.ErrorIs(t, err, ErrInvalidAction)

		res, err := h2.svc.Skip(h2.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"a"}})
		require.ErrorIs(t, err, ErrInvalidAction)
		assert.Equal(t, map[string]string{"a": "holds a lease for the rollout"}, res.Rejected)
	})
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

func TestRetryAction(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0, packagesNeed("2.0"))
	h.tick()

	id := h.openRollouts()[0].ID

	_, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: id})
	require.ErrorIs(t, err, ErrInvalidAction, "nothing failed")

	h.failLease("a")
	require.Equal(t, fleet.RolloutHalted, h.rollout(id).State)

	h.advance(time.Hour)
	h.tick()
	assert.Nil(t, h.assignment("b").Lease, "the cause is blocked")
	assert.Equal(t, "rollout halted: a: container did not start", h.assignment("b").Queue.Reason)

	res, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{
		Rollout: id, Hosts: []string{"a", "b"}, Reason: "fixed the config",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, res.Hosts)
	assert.Equal(t, map[string]string{"b": "has neither failed nor been skipped in the rollout"}, res.Rejected)

	r := h.rollout(id)
	assert.Equal(t, fleet.RolloutActive, r.State)
	assert.Empty(t, r.Reason)
	assert.Equal(t, fleet.HostPending, r.Host("a").State)
	assert.Empty(t, r.Host("a").Error)

	entry := h.store.lastAudit()
	assert.Equal(t, "action.retry", entry.Action)
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, map[string]any{"state": fleet.RolloutHalted, "reason": "a: container did not start"},
		entry.Detail["before"])

	// The agent has forgotten the failed operation by now.
	h.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
	h.tick()
	assert.NotNil(t, h.assignment("a").Lease)

	t.Run("a halted rollout whose failed host was skipped is reopened by retry", func(t *testing.T) {
		h2 := newHarness(t)
		h2.addHost("a", 0, packagesNeed("2.0"))
		h2.tick()

		id := h2.openRollouts()[0].ID
		h2.failLease("a")

		_, err := h2.svc.Skip(h2.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"a"}})
		require.NoError(t, err)
		h2.tick()
		assert.Equal(t, fleet.RolloutHalted, h2.rollout(id).State, "halt is lifted by retry only")

		_, err = h2.svc.Retry(h2.ctx, "alice", RolloutHostsRequest{Rollout: id})
		require.NoError(t, err)
		h2.tick()
		assert.Equal(t, fleet.RolloutCompleted, h2.rollout(id).State)

		// The skipped host still reports the need and is not served again.
		h2.advance(time.Hour)
		h2.tick()
		assert.Empty(t, h2.openRollouts())
		assert.Nil(t, h2.assignment("a").Lease)

		// Retry of the completed rollout lifts the skip, once.
		_, err = h2.svc.Retry(h2.ctx, "alice", RolloutHostsRequest{Rollout: id})
		require.NoError(t, err)
		assert.Equal(t, fleet.RolloutCompleted, h2.rollout(id).State, "completed is terminal")

		_, err = h2.svc.Retry(h2.ctx, "alice", RolloutHostsRequest{Rollout: id})
		require.ErrorIs(t, err, ErrInvalidAction, "nothing left to retry")
	})

	t.Run("unknown rollout", func(t *testing.T) {
		_, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: "nope"})
		require.ErrorIs(t, err, ErrUnknownRollout)
	})
}

func TestAbortAndRetryOfAnAbortedCause(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0, packagesNeed("2.0"))
	h.tick()

	id := h.openRollouts()[0].ID
	require.NotNil(t, h.assignment("a").Lease)

	_, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{})
	require.ErrorIs(t, err, ErrInvalidAction, "abort needs a rollout")

	_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: "nope"})
	require.ErrorIs(t, err, ErrUnknownRollout)

	res, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: id, Reason: "bad packages"})
	require.NoError(t, err)
	assert.Equal(t, id, res.Rollout)

	r := h.rollout(id)
	assert.Equal(t, fleet.RolloutAborted, r.State)
	assert.Equal(t, "bad packages", r.Reason)
	require.NotNil(t, r.EndedAt)
	assert.Equal(t, "action.abort", h.store.lastAudit().Action)

	_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: id})
	require.ErrorIs(t, err, ErrInvalidAction, "aborted is terminal")

	// The open lease finishes; the cause opens no new rollout.
	h.tick()
	require.NotNil(t, h.assignment("a").Lease)
	h.finish("a")
	assert.Nil(t, h.assignment("a").Lease)
	assert.Empty(t, h.openRollouts())
	assert.Nil(t, h.assignment("b").Lease)
	assert.Equal(t, fleet.RolloutAborted, h.rollout(id).State)

	// Retry lifts the block with a new rollout; the aborted one stays closed.
	res, err = h.svc.Retry(h.ctx, "bob", RolloutHostsRequest{Rollout: id, Reason: "fixed upstream"})
	require.NoError(t, err)
	require.NotEqual(t, id, res.Rollout)
	assert.Equal(t, fleet.RolloutAborted, h.rollout(id).State)

	reopened := h.rollout(res.Rollout)
	assert.Equal(t, r.Key, reopened.Key)
	assert.Equal(t, fleet.RolloutActive, reopened.State)
	assert.Equal(t, "bob", reopened.CreatedBy)
	assert.Equal(t, "action.retry", h.store.lastAudit().Action)

	_, err = h.svc.Retry(h.ctx, "bob", RolloutHostsRequest{Rollout: id})
	require.ErrorIs(t, err, ErrInvalidAction, "already open for the same cause")

	h.tick()
	assert.NotNil(t, h.assignment("b").Lease)
	assert.Equal(t, fleet.HostLeased, h.rollout(res.Rollout).Host("b").State)

	t.Run("abort cancels unanswered manual directives; a manual rollout cannot be retried", func(t *testing.T) {
		h2 := newHarness(t)
		h2.addHost("a", 0)

		res, err := h2.svc.Reboot(h2.ctx, "alice", RebootRequest{All: true})
		require.NoError(t, err)
		require.Len(t, h2.assignment("a").Directives, 1)

		_, err = h2.svc.Abort(h2.ctx, "alice", ScopeRequest{Rollout: res.Rollout})
		require.NoError(t, err)
		assert.Empty(t, h2.assignment("a").Directives)

		_, err = h2.svc.Retry(h2.ctx, "alice", RolloutHostsRequest{Rollout: res.Rollout})
		require.ErrorIs(t, err, ErrInvalidAction)
	})
}

func TestForgetAction(t *testing.T) {
	h := newHarness(t)
	h.addHost("busy", 0, packagesNeed("2.0"))
	h.addHost("gone", 0, packagesNeed("2.0"))
	h.addHost("held", 0)
	h.tick()

	_, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"held"}})
	require.NoError(t, err)
	h.tick()
	require.Len(t, h.assignment("held").Directives, 1)

	_, err = h.svc.Forget(h.ctx, "alice", HoldRequest{Hosts: []string{"nope"}})
	require.ErrorIs(t, err, ErrUnknownHost)

	res, err := h.svc.Forget(h.ctx, "alice", HoldRequest{Hosts: []string{"busy"}})
	require.ErrorIs(t, err, ErrInvalidAction)
	assert.Equal(t, map[string]string{"busy": "holds a lease"}, res.Rejected)
	assert.NotNil(t, h.store.host("busy"))

	res, err = h.svc.Forget(h.ctx, "alice", HoldRequest{Hosts: []string{"gone", "held", "busy"}, Reason: "decommissioned"})
	require.NoError(t, err)
	assert.Equal(t, []string{"gone", "held"}, res.Hosts)
	assert.Equal(t, map[string]string{"busy": "holds a lease"}, res.Rejected)
	assert.Nil(t, h.store.host("gone"))
	assert.Empty(t, h.assignment("held").Directives, "its directives are cancelled")

	entry := h.store.lastAudit()
	assert.Equal(t, "action.forget", entry.Action)
	assert.Equal(t, "held", entry.Target)

	// The rollout goes on without the forgotten host.
	h.tick()

	id := h.openRollouts()[0].ID
	assert.Equal(t, fleet.HostSkipped, h.rollout(id).Host("gone").State)

	h.finish("busy")
	assert.Equal(t, fleet.RolloutCompleted, h.rollout(id).State)
	assert.Len(t, h.svc.Plan().Hosts, 1)
}

func TestServiceAdoptsOnlyUnknownLeases(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.tick()

	// The lease fails; the agent keeps reporting the failed operation until
	// a response without the lease reaches it. It is not adopted again.
	first := h.assignment("a").Lease
	h.failLease("a")
	h.tick()
	h.tick()

	_, leases, _ := h.store.snapshot()
	require.Len(t, leases, 1)
	assert.Equal(t, fleet.LeaseFailed, leases[0].State)
	assert.Equal(t, first.ID, leases[0].ID)

	// A lost database: the controller has never seen the lease.
	need := packagesNeed("3.0")

	h.sync("a", func(st *protocol.SyncRequest) {
		st.Needs = []protocol.Need{need}
		st.Operation = &protocol.Operation{
			LeaseID: "from-before", Phase: protocol.PhaseStarting, Disrupted: true,
			Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepRunning}},
		}
	})
	h.tick()

	a := h.assignment("a")
	require.NotNil(t, a.Lease)
	assert.Equal(t, "from-before", a.Lease.ID)
	assert.Contains(t, h.store.auditActions(), "lease.adopted")

	// The row knows what the operation works on, but the lease is restated
	// without needs: an agent that already finished and forgot the operation
	// must not run it a second time.
	_, leases, _ = h.store.snapshot()
	require.Len(t, leases, 2)
	assert.Equal(t, []protocol.Need{need}, leases[1].Needs)
	assert.Empty(t, a.Lease.Needs)
}

// reportsNeed tells whether the host's last sync lists the need.
func (h *harness) reportsNeed(name, needID string) bool {
	return slices.ContainsFunc(h.store.host(name).Status.Needs, func(n protocol.Need) bool { return n.ID == needID })
}

func TestSkipOutlivesTheRollout(t *testing.T) {
	needs := []struct {
		name string
		need protocol.Need
	}{
		{"packages", packagesNeed("2.0")},
		{"reboot", rebootNeed()},
		{"image", imageNeed("c", imgLighthouse, digestA)},
	}

	// start skips "waiting" and serves "leased", so the rollout completes
	// with a skipped host that still reports the need.
	start := func(t *testing.T, need protocol.Need) (h *harness, id string) {
		t.Helper()

		h = newHarness(t)
		h.addHost("leased", 0, need)
		h.addHost("waiting", 0, need)
		h.tick()

		id = h.openRollouts()[0].ID

		_, err := h.svc.Skip(h.ctx, "alice", RolloutHostsRequest{
			Rollout: id, Hosts: []string{"waiting"}, Reason: "broken disk",
		})
		require.NoError(t, err)

		h.finish("leased")
		require.Equal(t, fleet.RolloutCompleted, h.rollout(id).State)
		require.True(t, h.reportsNeed("waiting", need.ID))

		return h, id
	}

	for _, tt := range needs {
		t.Run(tt.name+": the skipped host is not rolled once the rollout completes", func(t *testing.T) {
			h, id := start(t, tt.need)

			for range 5 {
				h.advance(2 * time.Second)
				h.tick()
				assert.Empty(t, h.openRollouts(), "the skipped host's need opens no rollout")
				assert.Nil(t, h.assignment("waiting").Lease)
			}

			// A restart remembers the skip as well.
			h.restart()
			h.advance(time.Hour)
			h.tick()
			assert.Empty(t, h.openRollouts())
			assert.Equal(t, "skipped: broken disk", h.rollout(id).Host("waiting").Error)
			assert.NotContains(t, h.store.auditActions(), "rollout.skip-lifted")
		})
	}

	t.Run("another host with the same need opens a rollout; the skipped host joins it as skipped", func(t *testing.T) {
		h, _ := start(t, packagesNeed("2.0"))
		h.addHost("late", 0, packagesNeed("2.0"))
		h.advance(2 * time.Second)
		h.tick()

		open := h.openRollouts()
		require.Len(t, open, 1)
		assert.Equal(t, fleet.HostSkipped, open[0].Host("waiting").State)
		assert.Equal(t, "skipped: broken disk", open[0].Host("waiting").Error)
		assert.Equal(t, fleet.HostLeased, open[0].Host("late").State)
		assert.Nil(t, h.assignment("waiting").Lease)

		h.finish("late")
		assert.Equal(t, fleet.RolloutCompleted, h.rollout(open[0].ID).State)

		h.advance(2 * time.Second)
		h.tick()
		assert.Empty(t, h.openRollouts(), "the skip moved on to the newer closed rollout")
		assert.Nil(t, h.assignment("waiting").Lease)
	})

	t.Run("a changed key is a new cause", func(t *testing.T) {
		h, _ := start(t, packagesNeed("2.0"))
		h.sync("waiting", func(st *protocol.SyncRequest) { st.Needs = []protocol.Need{packagesNeed("3.0")} })
		h.tick()

		require.Len(t, h.openRollouts(), 1)
		assert.NotNil(t, h.assignment("waiting").Lease)
	})

	t.Run("retry of the closed rollout lifts the skip", func(t *testing.T) {
		h, id := start(t, packagesNeed("2.0"))

		res, err := h.svc.Retry(h.ctx, "bob", RolloutHostsRequest{
			Rollout: id, Hosts: []string{"waiting", "leased"}, Reason: "disk replaced",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"waiting"}, res.Hosts)
		assert.Equal(t, map[string]string{"leased": "is not skipped in the rollout"}, res.Rejected)
		assert.Equal(t, fleet.RolloutCompleted, h.rollout(id).State, "the closed rollout stays closed")
		assert.Equal(t, "skip lifted: disk replaced", h.rollout(id).Host("waiting").Error)

		entry := h.store.lastAudit()
		assert.Equal(t, "action.retry", entry.Action)
		assert.Equal(t, "bob", entry.Actor)

		h.tick()
		require.Len(t, h.openRollouts(), 1)
		assert.NotNil(t, h.assignment("waiting").Lease)

		_, err = h.svc.Retry(h.ctx, "bob", RolloutHostsRequest{Rollout: id})
		require.ErrorIs(t, err, ErrInvalidAction, "no skip left to lift")
	})

	t.Run("the skip survives an aborted cause that is reopened and aborted again at once", func(t *testing.T) {
		h := newHarness(t)
		h.addHost("leased", 0, packagesNeed("2.0"))
		h.addHost("waiting", 0, packagesNeed("2.0"))
		h.tick()

		id := h.openRollouts()[0].ID

		_, err := h.svc.Skip(h.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"waiting"}})
		require.NoError(t, err)
		_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: id})
		require.NoError(t, err)
		h.finish("leased")

		// Reopened and aborted before any reconcile has seen the new rollout.
		res, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: id})
		require.NoError(t, err)
		assert.Equal(t, fleet.HostSkipped, h.rollout(res.Rollout).Host("waiting").State)

		_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: res.Rollout})
		require.NoError(t, err)

		res, err = h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: id})
		require.NoError(t, err)

		for range 3 {
			h.advance(2 * time.Second)
			h.tick()
			assert.Nil(t, h.assignment("waiting").Lease)
		}

		assert.Equal(t, fleet.HostSkipped, h.rollout(res.Rollout).Host("waiting").State)
		assert.Equal(t, fleet.RolloutCompleted, h.rollout(res.Rollout).State)
	})

	t.Run("retry of an open rollout takes a skip back only for a named host", func(t *testing.T) {
		h := newHarness(t)
		h.addHost("leased", 0, packagesNeed("2.0"))
		h.addHost("waiting", 0, packagesNeed("2.0"))
		h.tick()

		id := h.openRollouts()[0].ID

		_, err := h.svc.Skip(h.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"waiting"}})
		require.NoError(t, err)

		_, err = h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: id})
		require.ErrorIs(t, err, ErrInvalidAction, "nothing failed, and no host is named")
		assert.Equal(t, fleet.HostSkipped, h.rollout(id).Host("waiting").State)

		res, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"waiting"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"waiting"}, res.Hosts)
		assert.Equal(t, fleet.HostPending, h.rollout(id).Host("waiting").State)
	})

	t.Run("the skip ends when the host stops reporting the need", func(t *testing.T) {
		h, id := start(t, rebootNeed())

		// Someone rebooted the host by hand.
		h.sync("waiting", func(st *protocol.SyncRequest) { st.Needs = nil })
		h.tick()
		assert.Equal(t, "skip lifted: the host stopped reporting the need", h.rollout(id).Host("waiting").Error)
		assert.Contains(t, h.store.auditActions(), "rollout.skip-lifted")

		// A later reboot need is a new one.
		h.advance(time.Hour)
		h.sync("waiting", func(st *protocol.SyncRequest) { st.Needs = []protocol.Need{rebootNeed()} })
		h.tick()
		assert.NotNil(t, h.assignment("waiting").Lease)
	})

	t.Run("a held host reports nothing and keeps its skip", func(t *testing.T) {
		h, id := start(t, rebootNeed())

		h.sync("waiting", func(st *protocol.SyncRequest) {
			st.Needs = nil
			st.Hold = protocol.Hold{Local: true}
		})
		h.tick()
		assert.Equal(t, "skipped: broken disk", h.rollout(id).Host("waiting").Error)

		h.sync("waiting", func(st *protocol.SyncRequest) {
			st.Needs = []protocol.Need{rebootNeed()}
			st.Hold = protocol.Hold{}
		})
		h.tick()
		assert.Empty(t, h.openRollouts())
		assert.Nil(t, h.assignment("waiting").Lease)
	})
}

func TestForgetIsAuditedBeforeTheHostIsDeleted(t *testing.T) {
	t.Run("no audit entry, no delete", func(t *testing.T) {
		h := newHarness(t)
		h.addHost("gone", 0)
		h.store.setFailing(false, 1)

		res, err := h.svc.Forget(h.ctx, "alice", HoldRequest{Hosts: []string{"gone"}})
		require.ErrorIs(t, err, errWrite)
		require.NotNil(t, res)
		assert.Empty(t, res.Hosts)
		assert.NotNil(t, h.store.host("gone"), "a host is never deleted without an audit entry")
		assert.Empty(t, h.store.auditActions())
	})

	t.Run("a delete that fails after the audit entry is audited as failed", func(t *testing.T) {
		h := newHarness(t)
		h.addHost("gone", 0)
		h.store.failSkip, h.store.failNext = 1, 1

		_, err := h.svc.Forget(h.ctx, "alice", HoldRequest{Hosts: []string{"gone"}})
		require.ErrorIs(t, err, errWrite)
		assert.NotNil(t, h.store.host("gone"))
		assert.Equal(t, []string{"action.forget", "action.forget.failed"}, h.store.auditActions())
	})

	t.Run("a failure names the hosts that are already gone", func(t *testing.T) {
		h := newHarness(t)
		h.addHost("a", 0)
		h.addHost("b", 0)
		// Audit and delete of "a" pass, the audit entry of "b" fails.
		h.store.failSkip, h.store.failNext = 2, 1

		res, err := h.svc.Forget(h.ctx, "alice", HoldRequest{Hosts: []string{"a", "b"}})
		require.ErrorIs(t, err, errWrite)
		require.NotNil(t, res)
		assert.Equal(t, []string{"a"}, res.Hosts)
		assert.Nil(t, h.store.host("a"))
		assert.NotNil(t, h.store.host("b"))
		assert.Equal(t, []string{"action.forget"}, h.store.auditActions())
	})

	t.Run("directives that could not be cancelled are cancelled by the next reconcile", func(t *testing.T) {
		h := newHarness(t)
		h.addHost("gone", 0)

		_, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"gone"}})
		require.NoError(t, err)
		h.tick()
		require.Len(t, h.assignment("gone").Directives, 1)

		// Audit and delete pass, the cancellation fails.
		h.store.failSkip, h.store.failNext = 2, 1

		res, err := h.svc.Forget(h.ctx, "alice", HoldRequest{Hosts: []string{"gone"}})
		require.NoError(t, err, "the host is gone and audited")
		assert.Equal(t, []string{"gone"}, res.Hosts)
		assert.Len(t, h.assignment("gone").Directives, 1)

		h.tick()
		assert.Empty(t, h.assignment("gone").Directives)
	})
}

func TestHoldOfSeveralHostsReportsThePartialResult(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0)
	h.addHost("b", 0)
	h.addHost("c", 0)
	// Hold and audit of "a" pass, the hold of "b" fails.
	h.store.failSkip, h.store.failNext = 2, 1

	res, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a", "b", "c"}, Reason: "debugging"})
	require.ErrorIs(t, err, errWrite)
	require.NotNil(t, res, "the caller learns what was changed")
	assert.Equal(t, []string{"a"}, res.Hosts)
	assert.Contains(t, res.Message, "stopped at b")
	assert.True(t, h.store.host("a").HoldWanted)
	assert.False(t, h.store.host("b").HoldWanted)
	assert.False(t, h.store.host("c").HoldWanted)
	assert.Equal(t, []string{"action.hold"}, h.store.auditActions())
}

func TestReservedActorNames(t *testing.T) {
	tests := []struct{ actor, want string }{
		{"", "anonymous"},
		{"alice", "alice"},
		{"adopted", "adopted"},
		{"scheduler", "user:scheduler"},
		{"scheduler:adopted", "user:scheduler:adopted"},
		{"scheduler:x", "user:scheduler:x"},
		{"schedulers", "schedulers"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, actorName(tt.actor), "actor %q", tt.actor)
	}

	// A person who carries the adoption marker, or the name the marker had
	// before it was reserved, still gets a lease with its needs.
	for _, actor := range []string{grantedByAdoption, "adopted", actorScheduler} {
		t.Run(actor, func(t *testing.T) {
			h := newHarness(t)
			h.addHost("a", 0)

			res, err := h.svc.Reboot(h.ctx, actor, RebootRequest{All: true})
			require.NoError(t, err)

			dir := h.assignment("a").Directives[0]
			need := protocol.Need{
				ID: protocol.ManualNeedID(dir.ID), Kind: protocol.NeedReboot, DirectiveID: dir.ID,
				Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
			}

			h.sync("a", func(st *protocol.SyncRequest) {
				st.Needs = []protocol.Need{need}
				st.DirectiveAcks = []protocol.DirectiveAck{{ID: dir.ID, Status: protocol.DirectiveAccepted}}
			})
			h.tick()

			lease := h.assignment("a").Lease
			require.NotNil(t, lease)
			assert.Equal(t, []protocol.Need{need}, lease.Needs, "a manual lease is restated with its needs")

			_, leases, _ := h.store.snapshot()
			require.Len(t, leases, 1)
			assert.Equal(t, actorName(actor), leases[0].GrantedBy)
			assert.NotEqual(t, grantedByAdoption, leases[0].GrantedBy)
			assert.Equal(t, actorName(actor), h.rollout(res.Rollout).CreatedBy)
			assert.Equal(t, actorName(actor), h.store.lastAuditOf("action.reboot").Actor)
		})
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

	steady := []string{"rollouts:open", "directives:pending"}
	h.store.reads = nil

	for range 5 {
		h.advance(2 * time.Second)
		h.tick()
	}

	assert.Equal(t, steady, h.store.readKinds(), "a tick reads neither closed rollouts nor answered directives")
	assert.Equal(t, fleet.HostPending, h.rollout(res.Rollout).Host("b").State,
		"the answered directive of the open manual rollout is still known")
	assert.Equal(t, "host limit 1 of 1", h.assignment("b").Queue.Reason)

	// A restarted scheduler reads the history once and decides the same.
	h.restart()
	h.store.reads = nil
	h.advance(2 * time.Second)
	h.tick()
	assert.Equal(t, []string{"rollouts:open", "rollouts:all", "directives:all"}, h.store.readKinds())
	assert.Equal(t, fleet.HostPending, h.rollout(res.Rollout).Host("b").State)

	h.store.reads = nil
	h.advance(2 * time.Second)
	h.tick()
	assert.Equal(t, steady, h.store.readKinds())

	// The rollout runs to its end on what the scheduler remembers.
	h.finish("a")
	require.NotNil(t, h.assignment("b").Lease)
	assert.Equal(t, []protocol.Need{need}, h.assignment("b").Lease.Needs)

	h.finish("b")
	assert.Equal(t, fleet.RolloutCompleted, h.rollout(res.Rollout).State)

	h.store.reads = nil
	h.advance(2 * time.Second)
	h.tick()
	assert.Equal(t, steady, h.store.readKinds())
}

func TestAbortedCauseStaysClosedHoweverManyRolloutsFollow(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.tick()

	id := h.openRollouts()[0].ID

	_, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	h.finish("a")
	h.sync("a", func(st *protocol.SyncRequest) { st.Needs = []protocol.Need{packagesNeed("2.0")} })

	// Many rollouts of other causes come and go.
	ended := h.now
	changes := &store.Changes{}

	for i := range 700 {
		changes.Rollouts = append(changes.Rollouts, &fleet.Rollout{
			ID: fmt.Sprintf("old%04d", i), Kind: fleet.RolloutPackages, Key: fmt.Sprintf("packages:old%04d", i),
			State: fleet.RolloutCompleted, CreatedAt: h.now.Add(time.Duration(i) * time.Second), EndedAt: &ended,
		})
	}

	require.NoError(t, h.store.Commit(h.ctx, changes))

	h.restart()
	h.advance(time.Hour)
	h.tick()
	assert.Empty(t, h.openRollouts(), "the aborted cause stays closed")
	assert.Nil(t, h.assignment("a").Lease)

	// After a failed write the history is read again, with the same result.
	h.addHost("b", 0, rebootNeed())
	h.store.setFailing(false, 1)
	require.ErrorIs(t, h.svc.reconcile(h.ctx), errWrite)
	h.store.reads = nil
	h.tick()
	assert.Contains(t, h.store.readKinds(), "rollouts:all")
	require.Len(t, h.openRollouts(), 1)
	assert.Equal(t, keyReboot, h.openRollouts()[0].Key, "the aborted cause is still closed")
	assert.Nil(t, h.assignment("a").Lease)
}
