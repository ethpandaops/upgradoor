package scheduler

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The tests in this file replay the scenarios of the fourth review of the
// scheduler, one per finding. The review found them in the lists that a
// rollout handed to the rollouts after it; those lists are gone, and the
// scenarios are held against what can be seen: who is granted first, who
// waits and for whom. (Deleted with the lists: the checks of the lists
// themselves, of an empty list that the store gives back as none, and of
// rows whose list was recorded by one build and not by another.)

// beforeGenesis moves the builder's clock to d before genesis, with a beacon
// view as a node gives it then: epoch 0, nothing finalized, no liveness.
func beforeGenesis(b *worldBuilder, d time.Duration) {
	b.autoLive = false
	b.healthMod = func(n *fleet.NetworkHealth) {
		current, _ := n.EpochAt(b.w.Now)
		n.CurrentEpoch = current
		n.FinalizedEpoch = current - min(current, 2)
		n.FinalityLagEpochs = min(current, 2)
		n.LivenessKnown = false
		n.LivenessEpoch = 0
	}
	b.w.Now = testGenesis.Add(-d)
	b.refreshHealth()
}

// Finding 2: a run of healthy syncs that began before genesis is placed from
// genesis on. Before, the epoch of the run's first sync could not be placed,
// the first sync never moved while the run lasted, and the lease read "epoch
// timing unknown" until it failed at readinessTimeout.
func TestReview4Finding2HealthyRunThatBeganBeforeGenesis(t *testing.T) {
	b := newWorldBuilder(t)
	beforeGenesis(b, 10*time.Minute)

	b.host("a", 0).Status.Node = nodeWith(8, 8)
	b.host("b", 0).Status.Node = nodeWith(8, 8)
	b.wants("a", packagesNeed("2.0"))

	out := b.step()
	require.True(t, out.Plan.Gate.OK, "gate: %v", out.Plan.Gate.Reasons)
	require.Equal(t, []string{"a"}, b.leasedHosts())

	// The operation is done three minutes before genesis.
	b.advance(7 * time.Minute)
	b.finish("a")
	b.step()
	require.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	// A healthy sync every minute. The window is the chain's first epoch: at
	// six minutes past genesis the run is nine minutes old and still short of
	// it.
	for b.w.Now.Before(testGenesis.Add(6 * time.Minute)) {
		b.advance(time.Minute)
		b.step()

		l := b.lease("a")
		require.NotNil(t, l, "released at %s past genesis", b.w.Now.Sub(testGenesis))
		require.Equal(t, fleet.LeaseVerifying, l.State)
		assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", l.Readiness,
			"at %s past genesis", b.w.Now.Sub(testGenesis))
	}

	since := *b.lease("a").SyncedSince
	assert.Equal(t, testGenesis.Add(-2*time.Minute), since, "the run itself began before genesis")

	// Seven minutes past genesis the first epoch is over.
	b.advance(time.Minute)

	out = b.step()
	require.Nil(t, b.lease("a"))
	assert.Contains(t, auditActions(out), "lease.released")
	assert.Equal(t, "ready: node at head with peers", releasedReadiness(t, out))
	assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)

	// A run that begins after genesis is measured as before.
	t.Run("a run from after genesis", func(t *testing.T) {
		b := newWorldBuilder(t)
		beforeGenesis(b, time.Minute)

		b.host("a", 0).Status.Node = nodeWith(8, 8)
		b.host("b", 0).Status.Node = nodeWith(8, 8)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		b.advance(3 * time.Minute)
		b.finish("a")
		b.step()
		b.advance(time.Minute)
		b.step()
		require.Equal(t, testGenesis.Add(3*time.Minute), *b.lease("a").SyncedSince)

		b.advance(testEpoch - time.Second)
		b.step()
		require.NotNil(t, b.lease("a"))

		b.advance(time.Second)
		b.step()
		assert.Nil(t, b.lease("a"))
	})
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

// abortAndRetry aborts the rollout and reopens its cause, as a person does
// who stops a rollout and changes their mind. It returns the new rollout.
func (h *harness) abortAndRetry(id string) *fleet.Rollout {
	h.t.Helper()

	_, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: id, Reason: "wait"})
	require.NoError(h.t, err)

	res, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: id, Reason: "go on"})
	require.NoError(h.t, err)
	require.NotEqual(h.t, id, res.Rollout)

	return h.rollout(res.Rollout)
}

// review4Fleet stores twenty hosts without validators, h00 to h19, and zz,
// which alone reports the packages need of 2.0. With 21 hosts two may hold a
// lease at once.
func review4Fleet(t *testing.T) (h *harness, all []string) {
	t.Helper()

	h = newHarness(t)

	for i := range 20 {
		name := fmt.Sprintf("h%02d", i)
		h.addHost(name, 0)
		all = append(all, name)
	}

	h.addHost("zz", 0, packagesNeed("2.0"))
	all = append(all, "zz")

	h.tick()
	require.Equal(t, []string{"zz"}, h.leased())

	return h, all
}

// endedLease returns the lease of the host that ended last.
func (h *harness) endedLease(host string) *fleet.Lease {
	h.t.Helper()

	all, err := h.store.ListLeases(h.ctx, store.LeaseFilter{Host: host})
	require.NoError(h.t, err)

	var last *fleet.Lease

	for _, l := range all {
		if l.State.Terminal() && (last == nil || endedAfter(l, last)) {
			last = l
		}
	}

	require.NotNil(h.t, last, "no lease of %s has ended", host)

	return last
}

// Finding 1, first scenario: the rollout that a retry opens for an aborted
// cause waits for a lease in flight like any other. zz is out for the rollout
// of 2.0 when 3.0 appears: the successor waits for that lease. A person
// aborts the successor and retries it. Before, the new rollout knew nothing
// of the lease: h00 became its canary and went down next to zz.
func TestReview4Finding1RetryWaitsForALeaseInFlight(t *testing.T) {
	h, all := review4Fleet(t)
	first := h.openRollouts()[0].ID
	lease := h.running("zz")

	h.advance(time.Minute)
	h.reports("3.0", all...)
	h.tick()

	second := h.openRollout(packagesKey("3.0"))

	require.Equal(t, fleet.RolloutSuperseded, h.rollout(first).State)
	require.Equal(t, []string{"zz"}, h.leased())
	require.Empty(t, second.Canary)
	require.Equal(t, "after zz", queueReasons(h.svc.Plan())["h00"])

	third := h.abortAndRetry(second.ID)

	h.tick()

	third = h.rollout(third.ID)
	assert.Equal(t, []string{"zz"}, h.leased(), "no second host down next to the lease in flight")
	assert.Empty(t, third.Canary)
	assert.Equal(t, "after zz", queueReasons(h.svc.Plan())["h00"])
	assert.Equal(t, fleet.StageRest, third.Host("zz").Stage, "nobody knows yet how the lease ends")

	// The lease fails: zz is broken, and goes first in the reopened rollout.
	h.failLease("zz")

	third = h.rollout(third.ID)
	assert.Equal(t, brokenBy(lease, first), third.Host("zz").Error)
	assert.Equal(t, fleet.StageRepair, third.Host("zz").Stage)
	assert.Equal(t, fleet.RolloutActive, third.State)
	assert.Equal(t, "after zz", queueReasons(h.svc.Plan())["h00"])

	h.sync("zz", func(st *protocol.SyncRequest) { st.Operation = nil })
	h.tick()
	assert.Equal(t, []string{"zz"}, h.leased())

	// A restart in between changes nothing: all of it is in the lease table.
	t.Run("with a restart before the retry", func(t *testing.T) {
		h, all := review4Fleet(t)
		h.running("zz")

		h.advance(time.Minute)
		h.reports("3.0", all...)
		h.tick()

		second := h.openRollout(packagesKey("3.0"))

		_, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: second.ID})
		require.NoError(t, err)

		h.restart()

		res, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: second.ID})
		require.NoError(t, err)

		h.tick()
		assert.Equal(t, []string{"zz"}, h.leased())
		assert.Empty(t, h.rollout(res.Rollout).Canary)
		assert.Equal(t, "after zz", queueReasons(h.svc.Plan())["h00"])
	})
}

// Finding 1, second scenario: zz failed in the rollout of 2.0 and goes first
// in its successor. After abort and retry it still does. Before, zz was an
// ordinary member of the rest and h00 the canary.
func TestReview4Finding1BrokenHostGoesFirstAfterAbortAndRetry(t *testing.T) {
	h, all := review4Fleet(t)
	first := h.openRollouts()[0].ID

	h.failLease("zz")
	require.Equal(t, fleet.RolloutHalted, h.rollout(first).State)

	note := brokenBy(h.endedLease("zz").ID, first)

	h.advance(time.Minute)
	h.sync("zz", func(st *protocol.SyncRequest) { st.Operation = nil })
	h.reports("3.0", all...)

	// Nothing is granted for now, so that the order shows.
	_, err := h.svc.Pause(h.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)
	h.tick()

	second := h.openRollout(packagesKey("3.0"))
	require.Equal(t, note, second.Host("zz").Error)
	require.Equal(t, fleet.StageRepair, second.Host("zz").Stage)
	require.Empty(t, second.Canary)

	third := h.abortAndRetry(second.ID)

	h.tick()

	third = h.rollout(third.ID)
	assert.Equal(t, note, third.Host("zz").Error)
	assert.Equal(t, fleet.StageRepair, third.Host("zz").Stage)
	assert.Equal(t, fleet.StageRest, third.Host("h00").Stage)
	assert.Empty(t, third.Canary, "no canary while a broken host is still to be served")

	_, err = h.svc.Resume(h.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)
	h.tick()

	assert.Equal(t, []string{"zz"}, h.leased(), "the host this change broke goes first, alone")
	assert.Equal(t, "after zz", queueReasons(h.svc.Plan())["h00"])
}

// seesImage makes the hosts report the tag at a digest while they still run
// the old one.
func (h *harness) seesImage(digest string, hosts ...string) {
	h.t.Helper()

	for _, name := range hosts {
		h.sync(name, func(st *protocol.SyncRequest) {
			st.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digest, h.now)}
			st.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digest)}
			st.Operation = nil
		})
	}
}

// Finding 1, third scenario: a tag that moves to a digest whose latest
// rollout was aborted supersedes the open rollout and opens nothing. In the
// rollout that a later retry opens, the host that the superseded rollout
// broke goes first. Before, it was an ordinary member.
func TestReview4Finding1RetryAfterARolloutSupersededWithoutASuccessor(t *testing.T) {
	h := newHarness(t)
	hosts := []string{"a", "b", "c"}

	for _, name := range hosts {
		h.addHost(name, 0)
	}

	// Digest B is aborted before anybody was granted.
	_, err := h.svc.Pause(h.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)
	h.seesImage(digestB, hosts...)
	h.tick()

	abortedB := h.openRollout(imageKey(imgLighthouse, digestB))
	_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: abortedB.ID, Reason: "bad build"})
	require.NoError(t, err)
	_, err = h.svc.Resume(h.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)

	// The tag moves to A; its canary fails.
	h.advance(time.Minute)
	h.seesImage(digestA, hosts...)
	h.tick()

	onA := h.openRollout(imageKey(imgLighthouse, digestA))
	require.Equal(t, "a", onA.Canary, "an aborted rollout that broke nothing changes nothing")
	require.Equal(t, []string{"a"}, h.leased())
	h.failLease("a")

	// The tag moves back to B: the rollout of A is superseded, and nothing
	// opens for the aborted digest.
	h.advance(time.Minute)
	h.seesImage(digestB, hosts...)
	h.tick()
	require.Empty(t, h.openRollouts())
	require.Equal(t, fleet.RolloutSuperseded, h.rollout(onA.ID).State)

	res, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: abortedB.ID})
	require.NoError(t, err)

	h.tick()

	reopened := h.rollout(res.Rollout)
	assert.Equal(t, brokenBy(h.endedLease("a").ID, onA.ID), reopened.Host("a").Error)
	assert.Equal(t, fleet.StageRepair, reopened.Host("a").Stage)
	assert.Equal(t, []string{"a"}, h.leased())
	assert.Equal(t, "after a", queueReasons(h.svc.Plan())["b"])
}

// Finding 1, the rule behind it: what a rollout that a person aborts broke is
// broken for whatever follows, as with a superseded one: the rollout of the
// next digest, or the one a retry opens. Its lease in flight is waited for.
func TestReview4Finding1AnAbortedRolloutLeavesWhatItBroke(t *testing.T) {
	t.Run("the failed host goes first for the next digest", func(t *testing.T) {
		h := newHarness(t)
		hosts := []string{"a", "b", "c"}

		for _, name := range hosts {
			h.addHost(name, 0)
		}

		h.seesImage(digestA, hosts...)
		h.tick()
		require.Equal(t, []string{"a"}, h.leased())
		h.failLease("a")

		onA := h.openRollout(imageKey(imgLighthouse, digestA))
		_, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: onA.ID, Reason: "bad build"})
		require.NoError(t, err)

		h.advance(time.Minute)
		h.seesImage(digestB, hosts...)
		h.tick()

		onB := h.openRollout(imageKey(imgLighthouse, digestB))
		assert.Equal(t, fleet.StageRepair, onB.Host("a").Stage)
		assert.Equal(t, brokenBy(h.endedLease("a").ID, onA.ID), onB.Host("a").Error)
		assert.Equal(t, []string{"a"}, h.leased())
		assert.Equal(t, "after a", queueReasons(h.svc.Plan())["b"])
	})

	t.Run("the failed host goes first after a retry", func(t *testing.T) {
		h := newHarness(t)
		hosts := []string{"a", "b", "c"}

		for _, name := range hosts {
			h.addHost(name, 0)
		}

		h.seesImage(digestA, hosts...)
		h.tick()
		h.failLease("a")

		onA := h.openRollout(imageKey(imgLighthouse, digestA))
		reopened := h.abortAndRetry(onA.ID)

		h.advance(time.Minute)
		h.seesImage(digestA, hosts...)
		h.tick()

		reopened = h.rollout(reopened.ID)
		assert.Equal(t, fleet.StageRepair, reopened.Host("a").Stage)
		assert.Equal(t, []string{"a"}, h.leased())
		assert.Equal(t, "after a", queueReasons(h.svc.Plan())["b"])
	})

	t.Run("a lease in flight is waited for", func(t *testing.T) {
		h := newHarness(t)
		hosts := []string{"a", "b", "c"}

		for _, name := range hosts {
			h.addHost(name, 0)
		}

		h.seesImage(digestA, hosts...)
		h.tick()
		require.Equal(t, []string{"a"}, h.leased())

		lease := h.assignment("a").Lease
		onA := h.openRollout(imageKey(imgLighthouse, digestA))
		reopened := h.abortAndRetry(onA.ID)

		h.tick()
		assert.Equal(t, []string{"a"}, h.leased(), "the canary in flight is still the only host out")
		assert.Empty(t, h.rollout(reopened.ID).Canary)
		assert.Equal(t, "after a", queueReasons(h.svc.Plan())["b"])

		// It comes back ready, on the digest: the host is in order, and the
		// reopened rollout goes on with a canary of its own.
		h.sync("a", func(st *protocol.SyncRequest) {
			st.Containers = []protocol.Container{container("c", imgLighthouse, digestA, digestA, h.now)}
			st.Operation = &protocol.Operation{
				LeaseID: lease.ID, Phase: protocol.PhaseDone, Disrupted: true,
				Steps: []protocol.Step{{NeedID: lease.Needs[0].ID, Status: protocol.StepDone}},
			}
			st.Needs = nil
		})
		h.tick()
		h.advance(5 * time.Second)
		h.tick()

		for left := 2 * testEpoch; left > 0 && slices.Contains(h.leased(), "a"); left -= time.Minute {
			h.advance(time.Minute)
			h.tick()
		}

		reopened = h.rollout(reopened.ID)
		assert.Equal(t, fleet.LeaseReleased, h.endedLease("a").State)
		assert.Equal(t, fleet.HostDone, h.rollout(onA.ID).Host("a").State)
		assert.Equal(t, []string{"b"}, h.leased())
		assert.Equal(t, "b", reopened.Canary)
	})

	t.Run("an open rollout of the same group waits for it at once", func(t *testing.T) {
		// Two packages rollouts side by side: a and b want 2.0, c and d 3.0.
		h := newHarness(t)

		for _, name := range []string{"a", "b"} {
			h.addHost(name, 0, packagesNeed("2.0"))
		}

		for _, name := range []string{"c", "d"} {
			h.addHost(name, 0, packagesNeed("3.0"))
		}

		set, err := h.svc.Settings(h.ctx)
		require.NoError(t, err)

		set.MaxUnavailablePercent = 100
		require.NoError(t, h.svc.UpdateSettings(h.ctx, "alice", set))

		// While both are open, each goes its own way.
		h.tick()
		require.Equal(t, []string{"a", "c"}, h.leased())

		on2 := h.openRollout(packagesKey("2.0"))
		h.running("a")
		h.running("c")

		// 2.0 is aborted while a is out for it.
		_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: on2.ID})
		require.NoError(t, err)

		// The canary of 3.0 comes back; nobody follows while a is still out.
		h.finish("c")
		assert.Equal(t, []string{"a"}, h.leased())
		assert.Equal(t, "after a", queueReasons(h.svc.Plan())["d"])
	})
}

// Finding 1, the same by a reconcile: a rollout that is superseded while
// another rollout of its group is open already leaves its lease in flight to
// that one. Before, only a rollout opened in the same reconcile knew of it.
func TestReview4Finding1SupersedeNextToAnOpenRollout(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100

	for _, name := range []string{"a", "b"} {
		b.host(name, 0)
		b.wants(name, packagesNeed("2.0"))
	}

	for _, name := range []string{"c", "d"} {
		b.host(name, 0)
		b.wants(name, packagesNeed("3.0"))
	}

	b.step()
	require.Equal(t, []string{"a", "c"}, b.leasedHosts())

	on2 := b.mustRollout(packagesKey("2.0")).ID
	lease := b.lease("a").ID
	b.report("a", protocol.PhaseApplying, true)
	b.report("c", protocol.PhaseApplying, true)

	// b now wants 3.0 as well, and a (in the middle of 2.0) reports nothing:
	// nobody asks for 2.0 any more.
	b.advance(time.Minute)
	b.get("b").Status.Needs = []protocol.Need{packagesNeed("3.0")}
	b.get("a").Status.Needs = nil

	out := b.step()

	require.Equal(t, fleet.RolloutSuperseded, b.closed(packagesKey("2.0")).State)
	assert.Equal(t, "after a", queueReasons(out.Plan)["b"])

	// The lease fails; a reports the need of 3.0 and goes first there.
	b.advance(time.Minute)
	b.report("a", protocol.PhaseFailed, true)
	b.get("a").Status.Needs = []protocol.Need{packagesNeed("3.0")}
	out = b.step()

	on3 := b.mustRollout(packagesKey("3.0"))
	assert.Equal(t, brokenBy(lease, on2), on3.Host("a").Error)
	assert.Equal(t, fleet.StageRepair, on3.Host("a").Stage)
	assert.Equal(t, fleet.RolloutActive, on3.State)
	assert.Equal(t, "after a", queueReasons(out.Plan)["b"])
}

// legacyRollout is a superseded packages rollout of 2.0 as an earlier build
// left it: its members never settled after it was closed.
func legacyRollout(b *worldBuilder, members ...fleet.RolloutHost) *fleet.Rollout {
	ended := b.w.Now.Add(-24 * time.Hour)

	return &fleet.Rollout{
		ID: "old", Kind: fleet.RolloutPackages, Key: packagesKey("2.0"), Title: "packages: docker-ce 2.0",
		State: fleet.RolloutSuperseded, Reason: "newer package versions appeared",
		CreatedBy: actorScheduler, CreatedAt: ended.Add(-time.Hour), UpdatedAt: ended, EndedAt: &ended,
		Hosts: members,
	}
}

// legacyLease is an ended lease of the packages need of 2.0 for the rollout
// "old", as the lease table of any build has it.
func legacyLease(b *worldBuilder, id, host string, state fleet.LeaseState, step protocol.StepStatus) *fleet.Lease {
	need := packagesNeed("2.0")
	ended := b.w.Now.Add(-25 * time.Hour)

	return &fleet.Lease{
		ID: id, Host: host, State: state,
		Needs: []protocol.Need{need}, Rollouts: map[string]string{need.ID: "old"},
		GrantedAt: ended.Add(-10 * time.Minute), ExpiresAt: ended.Add(20 * time.Minute), EndedAt: &ended,
		Operation: &protocol.Operation{
			LeaseID: id, Phase: protocol.PhaseDone, Disrupted: true,
			Steps: []protocol.Step{{NeedID: need.ID, Status: step}},
		},
	}
}

// Finding 3: the rows of a closed rollout say nothing about who is broken.
// The previous builds did not settle the members of a rollout after it was
// closed: a member whose lease ended long ago is still `leased`. Before,
// what such a rollout left was computed from its members: the lease counted
// as in doubt, then as lost, and a healthy host went first in the next
// rollout. Now only the lease table is asked, which every build wrote alike.
func TestReview4Finding3RowsOfAClosedRolloutBreakNoHealthyHost(t *testing.T) {
	build := func(t *testing.T) *worldBuilder {
		t.Helper()

		b := newWorldBuilder(t)
		b.w.Settings.Paused = true

		for _, name := range []string{"done", "failed", "healthy", "repaired", "waiting"} {
			b.host(name, 0)
			b.wants(name, packagesNeed("3.0"))
		}

		b.w.Closed = []*fleet.Rollout{legacyRollout(b,
			fleet.RolloutHost{Host: "done", State: fleet.HostDone, LeaseID: "l-done"},
			fleet.RolloutHost{Host: "failed", State: fleet.HostFailed, LeaseID: "l-failed", Error: "boom"},
			// Served after the rollout was closed; nobody wrote the outcome.
			fleet.RolloutHost{Host: "healthy", State: fleet.HostLeased, LeaseID: "l-gone"},
			// Failed in an older rollout and repaired in this one.
			fleet.RolloutHost{Host: "repaired", State: fleet.HostDone, LeaseID: "l-repair"},
			fleet.RolloutHost{Host: "waiting", State: fleet.HostPending},
		)}

		older := legacyLease(b, "l-older", "repaired", fleet.LeaseFailed, protocol.StepFailed)
		before := older.EndedAt.Add(-time.Hour)
		older.EndedAt = &before
		older.Rollouts = map[string]string{packagesNeed("2.0").ID: "older"}

		b.w.Ended = []*fleet.Lease{
			legacyLease(b, "l-done", "done", fleet.LeaseReleased, protocol.StepDone),
			legacyLease(b, "l-failed", "failed", fleet.LeaseFailed, protocol.StepFailed),
			legacyLease(b, "l-gone", "healthy", fleet.LeaseReleased, protocol.StepDone),
			older,
			legacyLease(b, "l-repair", "repaired", fleet.LeaseReleased, protocol.StepDone),
		}

		return b
	}

	stages := func(r *fleet.Rollout) map[string]fleet.Stage {
		out := make(map[string]fleet.Stage, len(r.Hosts))
		for i := range r.Hosts {
			out[r.Hosts[i].Host] = r.Hosts[i].Stage
		}

		return out
	}

	b := build(t)
	b.step()

	r := b.mustRollout(packagesKey("3.0"))
	assert.Equal(t, map[string]fleet.Stage{
		"done": fleet.StageRest, "failed": fleet.StageRepair, "healthy": fleet.StageRest,
		"repaired": fleet.StageRest, "waiting": fleet.StageRest,
	}, stages(r), "what the lease table says, and no more")
	assert.Equal(t, brokenBy("l-failed", "old"), r.Host("failed").Error)
	assert.Empty(t, r.Canary, "no canary while the broken host is still to be served")

	// A lease of such a rollout that is still open is in flight, as ever: the
	// build was replaced while the host was out.
	t.Run("a lease that is still open is in flight", func(t *testing.T) {
		b := build(t)
		b.w.Ended = slices.DeleteFunc(b.w.Ended, func(l *fleet.Lease) bool {
			return l.ID == "l-gone" || l.ID == "l-failed"
		})
		b.w.Closed[0].Host("failed").State = fleet.HostDone
		b.w.Leases = []*fleet.Lease{{
			ID: "l-gone", Host: "healthy", State: fleet.LeaseRunning,
			Needs:     []protocol.Need{packagesNeed("2.0")},
			Rollouts:  map[string]string{packagesNeed("2.0").ID: "old"},
			GrantedAt: b.w.Now.Add(-time.Minute), ExpiresAt: b.w.Now.Add(time.Hour),
			Operation: &protocol.Operation{LeaseID: "l-gone", Phase: protocol.PhaseApplying, Disrupted: true},
		}}
		b.get("healthy").Status.Operation = b.w.Leases[0].Operation
		b.w.Settings.Paused = false

		out := b.step()
		r := b.mustRollout(packagesKey("3.0"))

		assert.Equal(t, []string{"healthy"}, b.leasedHosts(), "nobody next to the lease in flight")
		assert.Empty(t, r.Canary)
		assert.Equal(t, "after healthy", queueReasons(out.Plan)["done"])
		assert.Equal(t, fleet.StageRest, r.Host("healthy").Stage)

		// It fails: now the host is broken, and its member in the old rollout
		// has the outcome.
		b.advance(time.Minute)
		b.get("healthy").Status.Operation = &protocol.Operation{
			LeaseID: "l-gone", Phase: protocol.PhaseFailed, Disrupted: true, Error: "boom",
		}
		b.step()

		r = b.mustRollout(packagesKey("3.0"))
		assert.Equal(t, fleet.StageRepair, r.Host("healthy").Stage)
		assert.Equal(t, []string{"healthy"}, b.leasedHosts(), "it goes first")
		assert.Equal(t, fleet.HostFailed, b.closed(packagesKey("2.0")).Host("healthy").State)
	})

	// A member that is still `leased` in the closed rollout by a lease that
	// failed after the rollout was closed: the lease says so, the row does
	// not.
	t.Run("a lease that failed after the rollout was closed", func(t *testing.T) {
		b := build(t)
		b.w.Ended[2] = legacyLease(b, "l-gone", "healthy", fleet.LeaseFailed, protocol.StepFailed)
		b.step()

		r := b.mustRollout(packagesKey("3.0"))
		assert.Equal(t, fleet.StageRepair, r.Host("healthy").Stage)
		assert.Equal(t, brokenBy("l-gone", "old"), r.Host("healthy").Error)
	})

	// A lease that nobody has (a lost database) breaks nobody.
	t.Run("without a lease table nobody is broken", func(t *testing.T) {
		b := build(t)
		b.w.Ended = nil
		b.step()

		r := b.mustRollout(packagesKey("3.0"))

		for i := range r.Hosts {
			assert.NotEqual(t, fleet.StageRepair, r.Hosts[i].Stage, r.Hosts[i].Host)
		}

		assert.NotEmpty(t, r.Canary)
	})

	// It makes no difference how the old rollout was closed.
	for _, state := range []fleet.RolloutState{fleet.RolloutAborted, fleet.RolloutCompleted} {
		t.Run("the old rollout is "+string(state), func(t *testing.T) {
			b := build(t)
			b.w.Closed[0].State = state
			b.step()

			if state == fleet.RolloutAborted {
				// An aborted cause stays closed for its own key only.
				require.NotNil(t, b.rollout(packagesKey("3.0")))
			}

			r := b.mustRollout(packagesKey("3.0"))
			assert.Equal(t, fleet.StageRepair, r.Host("failed").Stage)
			assert.Equal(t, fleet.StageRest, r.Host("healthy").Stage)
		})
	}
}

// review4Restarted plays the scenario of finding 4 up to the retry and
// returns the rollout that the retry opens, after one reconcile.
//
// zz fails in the rollout of 2.0, goes first in the rollout of 3.0 and is out
// for its repair there when 4.0 appears. The rollout of 3.0 is superseded
// with zz's lease open; nothing opens, because 4.0 was aborted before. zz
// comes back ready with its step superseded. Then somebody retries 4.0.
func review4Restarted(t *testing.T, restart bool) (h *harness, reopened *fleet.Rollout, broke string) {
	t.Helper()

	h, all := review4Fleet(t)
	first := h.openRollouts()[0].ID

	// 4.0 was seen once and aborted.
	_, err := h.svc.Pause(h.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)
	h.reports("4.0", "h00")
	h.tick()

	aborted := h.openRollout(packagesKey("4.0"))
	_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: aborted.ID})
	require.NoError(t, err)
	h.sync("h00", func(st *protocol.SyncRequest) { st.Needs = nil })
	_, err = h.svc.Resume(h.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)

	// zz fails for 2.0 and goes first for 3.0.
	h.failLease("zz")
	broke = brokenBy(h.endedLease("zz").ID, first)

	h.advance(time.Minute)
	h.sync("zz", func(st *protocol.SyncRequest) { st.Operation = nil })
	h.reports("3.0", all...)
	h.tick()

	second := h.openRollout(packagesKey("3.0"))
	require.Equal(t, fleet.StageRepair, second.Host("zz").Stage)
	require.Equal(t, []string{"zz"}, h.leased())

	lease := h.running("zz")

	// 4.0 appears while zz is out.
	h.advance(time.Minute)
	h.reports("4.0", all...)
	h.tick()
	require.Empty(t, h.openRollouts(), "4.0 was aborted: nothing opens")
	require.Equal(t, fleet.RolloutSuperseded, h.rollout(second.ID).State)

	// zz comes back: it stopped its containers, but the step for 3.0 was
	// overtaken. It passes readiness.
	h.sync("zz", func(st *protocol.SyncRequest) {
		st.Operation = &protocol.Operation{
			LeaseID: lease, Phase: protocol.PhaseDone, Disrupted: true,
			Steps: []protocol.Step{{NeedID: packagesNeed("3.0").ID, Status: protocol.StepSuperseded}},
		}
	})
	h.tick()
	h.advance(5 * time.Second)
	h.tick()

	for left := 2 * testEpoch; left > 0 && len(h.leased()) > 0; left -= time.Minute {
		h.advance(time.Minute)
		h.tick()
	}

	require.Empty(t, h.leased())
	require.Equal(t, "lease.released", h.store.lastAuditOf("lease.released").Action)
	require.Equal(t, fleet.HostPending, h.rollout(second.ID).Host("zz").State, "its step was not done")

	if restart {
		h.restart()
	}

	res, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: aborted.ID})
	require.NoError(t, err)

	_, err = h.svc.Pause(h.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)
	h.tick()

	return h, h.rollout(res.Rollout), broke
}

// Finding 4: what a rollout that is opened after a restart knows is what it
// would have known without the restart. Before, an empty list came back from
// the store as "recorded by an older build", and the restarted scheduler
// computed it anew, differently. Now there is nothing to come back: both
// read the lease table. In it, the lease that zz was released from says
// nothing about the packages (its step was overtaken), so zz's last word on
// them is still the failure for 2.0: it goes first for 4.0.
func TestReview4Finding4SameAfterARestart(t *testing.T) {
	running, runningRollout, broke := review4Restarted(t, false)
	restarted, restartedRollout, _ := review4Restarted(t, true)

	for name, r := range map[string]*fleet.Rollout{"running": runningRollout, "restarted": restartedRollout} {
		assert.Equal(t, fleet.StageRepair, r.Host("zz").Stage, "%s: no lease has repaired zz", name)
		assert.Equal(t, broke, r.Host("zz").Error, name)
		assert.Empty(t, r.Canary, name)
	}

	// Everything the two schedulers wrote is the same.
	assert.Equal(t, running.store.dump(), restarted.store.dump())
}

// The case the fourth review could not reach: a lease expires as not started,
// its rollout is superseded, and then the agent turns out to have started
// after all. The lease is taken up again; it is a lease of the superseded
// rollout in flight like one that was open at the supersede. Before, it was
// in nobody's list, and the successor leased its canary next to it.
func TestReview4LeaseReopenedAfterItsRolloutWasSuperseded(t *testing.T) {
	// reopened plays it up to the reconcile that takes the lease up again.
	reopened := func(t *testing.T) (b *worldBuilder, first, lease, need string, out *Outcome) {
		t.Helper()

		b = newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 100

		for _, name := range []string{"a", "b", "c"} {
			b.host(name, 10)
			b.wantsImage(name, imgLighthouse, digestA)
		}

		b.step()
		require.Equal(t, []string{"a"}, b.leasedHosts())

		keyA, keyB := imageKey(imgLighthouse, digestA), imageKey(imgLighthouse, digestB)
		first = b.mustRollout(keyA).ID
		lease = b.lease("a").ID
		need = b.lease("a").Needs[0].ID

		// The agent says nothing of the lease until after the grace period.
		b.w.Settings.Paused = true
		b.advance(b.w.Settings.LeaseTTL.Std() + leaseStartGrace)

		out = b.step()
		require.Contains(t, auditActions(out), "lease.expired")
		require.Equal(t, fleet.HostPending, b.mustRollout(keyA).Host("a").State)

		// The tag moves: the rollout is superseded with nobody out. A lease
		// that expired stopped nothing: "a" is an ordinary host.
		b.advance(time.Minute)
		seesDigest(b, digestB, "b", "c")
		b.step()

		require.Equal(t, fleet.RolloutSuperseded, b.closed(keyA).State)
		require.Equal(t, fleet.StageRest, b.mustRollout(keyB).Host("a").Stage)

		// The agent did start: its clock is slow.
		b.advance(5 * time.Second)
		b.get("a").Status.Operation = &protocol.Operation{
			LeaseID: lease, Phase: protocol.PhaseApplying, Disrupted: true,
			Steps: []protocol.Step{{NeedID: need, Status: protocol.StepRunning}},
		}
		b.w.Settings.Paused = false

		out = b.step()
		require.Contains(t, auditActions(out), "lease.reopened")

		return b, first, lease, need, out
	}

	keyA, keyB := imageKey(imgLighthouse, digestA), imageKey(imgLighthouse, digestB)

	b, first, lease, need, out := reopened(t)
	r := b.mustRollout(keyB)

	assert.Equal(t, []string{"a"}, b.leasedHosts(), "no canary next to the lease of the old rollout")
	assert.Empty(t, r.Canary)
	assert.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(out.Plan))

	// It fails: the host is broken and goes first.
	b.advance(time.Minute)
	b.get("a").Status.Operation = &protocol.Operation{
		LeaseID: lease, Phase: protocol.PhaseFailed, Disrupted: true, Error: "container did not start",
		Steps: []protocol.Step{{NeedID: need, Status: protocol.StepFailed}},
	}

	out = b.step()
	r = b.mustRollout(keyB)

	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, brokenBy(lease, first), r.Host("a").Error)
	assert.Equal(t, fleet.StageRepair, r.Host("a").Stage)
	assert.Equal(t, fleet.RolloutActive, r.State)
	assert.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(out.Plan))

	// Released instead, the host is in order and the successor goes on.
	t.Run("the reopened lease is released", func(t *testing.T) {
		b, _, lease, need, _ := reopened(t)

		b.get("a").Status.Operation = &protocol.Operation{
			LeaseID: lease, Phase: protocol.PhaseDone, Disrupted: true,
			Steps: []protocol.Step{{NeedID: need, Status: protocol.StepDone}},
		}
		seesDigestRunning(b, "a", digestA, digestB)
		b.step()
		b.advance(5 * time.Second)
		b.step()
		b.advance(2 * testEpoch)

		out := b.step()
		require.Contains(t, auditActions(out), "lease.released")
		assert.NotEqual(t, fleet.StageRepair, b.mustRollout(keyB).Host("a").Stage)
		assert.NotEmpty(t, b.mustRollout(keyB).Canary)
		assert.Len(t, b.leasedHosts(), 1, "one canary")
	})

	// A rollout that is opened only after the lease was taken up again waits
	// for it all the same: the lease is open, whoever looks.
	t.Run("a rollout opened afterwards", func(t *testing.T) {
		b, _, _, _, _ := reopened(t)

		b.advance(time.Minute)
		seesDigest(b, digestC, "b", "c")

		out := b.step()
		require.Equal(t, fleet.RolloutSuperseded, b.closed(keyB).State)
		assert.Equal(t, []string{"a"}, b.leasedHosts())
		assert.Empty(t, b.mustRollout(imageKey(imgLighthouse, digestC)).Canary)
		assert.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(out.Plan))
		assert.Equal(t, fleet.RolloutSuperseded, b.closed(keyA).State)
	})
}
