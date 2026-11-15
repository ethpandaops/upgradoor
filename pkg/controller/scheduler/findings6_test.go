package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The tests in this file replay the findings of the sixth review. Ending a
// lease used to fail it on the spot while the agent went on working: the
// agent began steps under a lease that was over, the host's slot was given
// to another host while it was still down, the result of the operation was
// thrown away, and a busy agent's host was granted the same work again every
// leaseTtl. Now ending a lease is a request: the lease stays open until the
// agent has stopped, and the host is held.

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

// agentHeld makes the host's agent report the controller hold, as it does
// once it has the directive: a held host reports no needs.
func (r *replay) agentHeld(host string) {
	r.t.Helper()

	r.sync(host, func(st *protocol.SyncRequest) {
		st.Hold.Controller = true
		st.Needs = nil
	})
}

// unhold lifts the hold of a host; its agent reports the needs again.
func (r *replay) unhold(host string, needs ...protocol.Need) {
	r.t.Helper()
	r.fresh()

	_, err := r.svc.Unhold(r.ctx, "alice", HoldRequest{Hosts: []string{host}})
	require.NoError(r.t, err)

	r.sync(host, func(st *protocol.SyncRequest) {
		st.Hold.Controller = false
		st.Needs = needs
		st.Operation = nil
	})
	r.dump()
}

// grantsTo counts the leases the scheduler granted to the host.
func (r *replay) grantsTo(host string) int {
	r.t.Helper()

	all, err := r.store.ListLeases(r.ctx, store.LeaseFilter{Host: host})
	require.NoError(r.t, err)

	return len(all)
}

// mixedFleet is three hosts without validators that all want the packages of
// 2.0; a also wants the image at digest A. With three hosts one may hold a
// lease at a time. a is granted both needs in one lease and reports it
// running.
func mixedFleet(t *testing.T, restart bool) (r *replay, lease string) {
	t.Helper()

	r = newReplay(t, restart)

	for _, name := range []string{"a", "b", "c"} {
		r.addHost(name, 0)
	}

	r.sync("a", func(st *protocol.SyncRequest) {
		st.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestA, r.now)}
		st.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digestA), packagesNeed("2.0")}
	})
	r.reports("2.0", "b", "c")
	r.pass()
	require.Equal(t, []string{"a"}, r.leased())

	lease = r.running("a")
	r.pass()
	require.Len(t, r.storedLease(lease).Needs, 2)
	require.Equal(t, fleet.LeaseRunning, r.storedLease(lease).State)

	return r, lease
}

// M1 and M2: ending a running lease ends nothing by itself. The lease stays
// open and keeps the host's slot, the agent is told to stop, the host is
// held; the lease ends when the agent reports, and what the agent reports is
// what counts.
func TestReview6EndingALeaseIsARequestTheAgentCarriesOut(t *testing.T) {
	play := func(t *testing.T, restart bool) *replay {
		t.Helper()

		r, lease := mixedFleet(t, restart)
		packages := packagesNeed("2.0")
		ri := r.openRollout(imageKey(imgLighthouse, digestA)).ID
		rp := r.openRollout(packagesKey("2.0")).ID

		require.False(t, r.assignment("a").Lease.Cancel)

		res, err := r.endLeaseID("alice", lease)
		require.NoError(t, err)
		assert.Equal(t, []string{"a"}, res.Hosts)
		assert.Contains(t, res.Message, "is ending; the host is on hold")

		// The lease is open, marked, and restated to the agent with the mark.
		l := r.storedLease(lease)
		assert.Equal(t, fleet.LeaseRunning, l.State)
		require.NotNil(t, l.Cancelled)
		assert.Equal(t, "alice", l.Cancelled.By)
		assert.Equal(t, fleet.LeaseRunning, l.Cancelled.State)
		assert.Equal(t, "taken away", l.Cancelled.Reason)
		assert.True(t, r.assignment("a").Lease.Cancel)

		// The host is held, in the transaction of the mark.
		host := r.store.host("a")
		assert.True(t, host.HoldWanted)
		assert.Equal(t, "lease ended by operator", host.HoldReason)

		entry := r.store.lastAuditOf("action.end-lease")
		assert.Equal(t, "alice", entry.Actor)
		assert.Equal(t, "a", entry.Target)
		assert.Equal(t, lease, entry.Detail[keyLease])
		assert.Equal(t, "taken away", entry.Detail[keyReason])
		assert.NotNil(t, entry.Detail["hold"])

		// M2: while the agent works the host is down, and its slot is taken.
		for range 5 {
			r.advance(time.Minute)
			r.pass()
		}

		assert.Equal(t, []string{"a"}, r.leased(), "nobody is granted the slot of a host that is still down")
		assert.Equal(t, 1, r.svc.Plan().Budget.LeasedHosts)
		assert.Equal(t, SlotLeased, r.svc.Plan().Hosts["a"].Slot)
		assert.True(t, r.assignment("a").Lease.Cancel, "restated on every sync")
		assert.Empty(t, r.store.lastAuditOf("lease.failed").Action, "no lease has failed")

		// M1: the agent completed the step in progress (the image) and did not
		// begin the packages. Its report ends the lease.
		r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone, protocol.StepCancelled)
		r.agentHeld("a")
		r.pass()

		l = r.storedLease(lease)
		assert.Equal(t, fleet.LeaseFailed, l.State)
		assert.Equal(t, "ended by operator", l.Error)
		assert.Equal(t, protocol.StepCancelled, stepStatus(l.Operation, packages.ID), "the report is on record")
		assert.Nil(t, r.assignment("a").Lease)

		// The image was done and the host was never shown to be back: its
		// member failed, like that of a lease that fails while it is verified.
		// The packages were not begun: their rollout halts as for any failed
		// lease, and that says nothing about the host.
		assert.Equal(t, fleet.RolloutHalted, r.rollout(ri).State)
		assert.Equal(t, "a: ended by operator", r.rollout(ri).Reason)
		assert.Equal(t, fleet.HostFailed, r.rollout(ri).Host("a").State)
		assert.Equal(t, lease, r.rollout(ri).Host("a").LeaseID)
		assert.Equal(t, fleet.RolloutHalted, r.rollout(rp).State)
		assert.Equal(t, "a: ended by operator", r.rollout(rp).Reason)
		assert.Equal(t, fleet.HostFailed, r.rollout(rp).Host("a").State)

		ld := newLedger([]*fleet.Lease{l})
		assert.NotNil(t, ld.brokenSince("a", prefixImageKey+imgLighthouse), "applied, never shown ready")
		assert.Nil(t, ld.brokenSince("a", groupPackages), "never begun")

		// The agent repeats its report until it sees a response without the
		// lease. Nothing is taken up again.
		r.advance(time.Minute)
		r.pass()
		assert.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)
		assert.Empty(t, r.leased())

		// Retried, the rollout goes on without the host: it is held, so it
		// holds nobody back, and it is not broken.
		r.retry(rp)
		r.pass()
		assert.Equal(t, []string{"b"}, r.leased())
		assert.NotContains(t, queueReasons(r.svc.Plan()), "a", "a held host reports no need")
		r.done("b")
		r.done("c")

		// Unheld, it is an ordinary host again for the packages: the step
		// that was never begun did not break it.
		r.unhold("a", packages)
		r.pass()
		assert.Equal(t, []string{"a"}, r.leased())

		m := r.rollout(rp).Host("a")
		assert.NotEqual(t, fleet.StageRepair, m.Stage)
		assert.Empty(t, m.Error)
		assert.Equal(t, 2, r.grantsTo("a"))

		return r
	}

	play(t, false)
	sameAfterRestarts(t, play)
}

// M3: a lease that the agent has not started can be ended too. It is no
// longer offered, so an agent that holds it unstarted declines it and one
// that never had it never gets it; the host is held and is not granted the
// same work again, however long its agent is busy with something else.
func TestReview6EndingAGrantedLease(t *testing.T) {
	granted := func(t *testing.T, restart bool) (r *replay, lease, rollout string) {
		t.Helper()

		r = newReplay(t, restart)

		for _, name := range []string{"a", "b", "c"} {
			r.addHost(name, 0)
		}

		r.reports("2.0", "a", "b", "c")
		r.pass()
		require.Equal(t, []string{"a"}, r.leased())

		lease = r.assignment("a").Lease.ID
		rollout = r.openRollout(packagesKey("2.0")).ID

		_, err := r.endLeaseID("alice", lease)
		require.NoError(t, err)

		l := r.storedLease(lease)
		require.Equal(t, fleet.LeaseGranted, l.State, "open until the agent says that it did not start")
		require.Equal(t, fleet.LeaseGranted, l.Cancelled.State)
		require.Nil(t, r.assignment("a").Lease, "no longer offered")
		require.Nil(t, r.assignment("a").Queue, "nor queued: it holds a lease")

		return r, lease, rollout
	}

	t.Run("the agent is busy and silent about the lease: no new grant, ever", func(t *testing.T) {
		play := func(t *testing.T, restart bool) *replay {
			t.Helper()

			r, lease, rollout := granted(t, restart)
			ttl := r.svc.opts.Defaults.LeaseTTL.Std()

			// The agent syncs without an operation for hours: it is busy, or it
			// never had the lease. The lease expires once, at its deadline, and
			// the host is not granted again.
			for elapsed := time.Duration(0); elapsed < 4*ttl; elapsed += time.Minute {
				r.advance(time.Minute)
				r.pass()

				if elapsed < ttl {
					require.Equal(t, []string{"a"}, r.leased(), "the slot is held until the lease has run out")
				}
			}

			assert.Equal(t, fleet.LeaseExpired, r.storedLease(lease).State)
			assert.Equal(t, 1, r.grantsTo("a"), "a held host is not granted again")
			assert.Equal(t, "held", queueReasons(r.svc.Plan())["a"])
			assert.Contains(t, r.leased(), "b", "nobody waits for a held host")

			// A skip works on it as on any held member.
			r.skip(rollout, "a")
			r.pass()
			assert.Equal(t, fleet.HostSkipped, r.rollout(rollout).Host("a").State)

			return r
		}

		play(t, false)
		sameAfterRestarts(t, play)
	})

	t.Run("the agent declines: the lease ends at once, as one that never started", func(t *testing.T) {
		r, lease, rollout := granted(t, false)

		r.reportOp("a", lease, protocol.PhaseDone, false, protocol.StepRefused)
		r.agentHeld("a")
		r.pass()

		assert.Equal(t, fleet.LeaseExpired, r.storedLease(lease).State)
		assert.Equal(t, fleet.RolloutActive, r.rollout(rollout).State, "nothing failed")
		assert.Equal(t, []string{"b"}, r.leased())
		assert.Equal(t, 1, r.grantsTo("a"))
	})

	t.Run("the agent had started: the lease is restated with the mark", func(t *testing.T) {
		r, lease, rollout := granted(t, false)

		r.reportOp("a", lease, protocol.PhaseApplying, true, protocol.StepRunning)
		r.pass()

		require.Equal(t, fleet.LeaseRunning, r.storedLease(lease).State)
		require.NotNil(t, r.assignment("a").Lease)
		assert.True(t, r.assignment("a").Lease.Cancel)
		assert.Equal(t, []string{"a"}, r.leased())

		// The one step had begun, so it is completed. The lease still ends as
		// ended by the operator: nobody waits for the readiness of a host that
		// a person took out. Its step was done, so the change was applied and
		// the host was never shown to be back: the member failed and the host
		// is broken, as if the lease had been ended while it was verified.
		r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone)
		r.agentHeld("a")
		r.pass()

		l := r.storedLease(lease)
		assert.Equal(t, fleet.LeaseFailed, l.State)
		assert.Equal(t, "ended by operator", l.Error)
		assert.Equal(t, fleet.HostFailed, r.rollout(rollout).Host("a").State)
		assert.Equal(t, lease, r.rollout(rollout).Host("a").LeaseID)
		assert.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State)
		assert.NotNil(t, newLedger([]*fleet.Lease{l}).brokenSince("a", groupPackages))
	})
}

// A lease that is being verified has its operation behind it: there is
// nothing the agent could stop. It ends in the next reconcile, and what that
// says is what a lease that fails while it is verified says: the change was
// applied and the host was never shown to be back, so it is broken.
func TestReview6EndingAVerifyingLease(t *testing.T) {
	play := func(t *testing.T, restart bool) *replay {
		t.Helper()

		r := newReplay(t, restart)

		for _, name := range []string{"a", "b", "c"} {
			r.addHost(name, 0)
		}

		r.reports("2.0", "a", "b", "c")
		r.pass()

		lease := r.assignment("a").Lease.ID
		rollout := r.openRollout(packagesKey("2.0")).ID

		r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone)
		r.pass()
		require.Equal(t, fleet.LeaseVerifying, r.storedLease(lease).State)

		_, err := r.endLeaseID("alice", lease)
		require.NoError(t, err)
		require.Equal(t, fleet.LeaseVerifying, r.storedLease(lease).State)
		require.Equal(t, fleet.LeaseVerifying, r.storedLease(lease).Cancelled.State)

		r.pass()

		l := r.storedLease(lease)
		assert.Equal(t, fleet.LeaseFailed, l.State)
		assert.Equal(t, "ended by operator", l.Error)
		assert.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State)
		assert.Equal(t, fleet.HostFailed, r.rollout(rollout).Host("a").State)
		assert.True(t, r.store.host("a").HoldWanted)

		// Held, it holds nobody back.
		r.agentHeld("a")
		r.retry(rollout)
		r.pass()
		assert.Equal(t, []string{"b"}, r.leased())

		// Unheld, it goes first: broken, like every host whose lease failed
		// while it was verified.
		r.unhold("a", packagesNeed("2.0"))
		r.pass()

		m := r.rollout(rollout).Host("a")
		assert.Equal(t, fleet.StageRepair, m.Stage)
		assert.Equal(t, brokenBy(lease, rollout), m.Error)
		assert.Equal(t, map[string]string{"a": "host limit 1 of 1", "c": "after a"}, queueReasons(r.svc.Plan()))

		r.done("b")
		assert.Equal(t, []string{"a"}, r.leased())

		return r
	}

	play(t, false)
	sameAfterRestarts(t, play)
}

// What ending a lease refuses, and that ending it twice is ending it once.
func TestReview6EndLeaseEdges(t *testing.T) {
	t.Run("twice is once", func(t *testing.T) {
		r, lease := mixedFleet(t, false)

		_, err := r.endLeaseID("alice", lease)
		require.NoError(t, err)

		entries := len(r.store.auditActions())
		before := r.storedLease(lease)

		res, err := r.endLeaseID("bob", lease)
		require.NoError(t, err)
		assert.Contains(t, res.Message, "already ended by alice")
		assert.Equal(t, []string{"a"}, res.Hosts)
		assert.Equal(t, before, r.storedLease(lease), "nothing is written")
		assert.Len(t, r.store.auditActions(), entries, "nor audited a second time")
	})

	t.Run("a hold that is there already stays, and its reason names the cancel too", func(t *testing.T) {
		r, lease := mixedFleet(t, false)

		_, err := r.svc.Hold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}, Reason: "disk is dying"})
		require.NoError(t, err)

		res, err := r.endLeaseID("alice", lease)
		require.NoError(t, err)
		assert.Contains(t, res.Message, "the host stays on hold")
		assert.True(t, r.store.host("a").HoldWanted)
		assert.Equal(t, "disk is dying; lease ended by operator", r.store.host("a").HoldReason)

		entry := r.store.lastAuditOf("action.end-lease")
		assert.Equal(t, map[string]any{
			keyBefore: map[string]any{keyHoldWanted: true, keyHoldReason: "disk is dying"},
			keyAfter:  map[string]any{keyHoldWanted: true, keyHoldReason: "disk is dying; lease ended by operator"},
		}, entry.Detail["hold"])

		// A second time changes nothing: the reason is not extended twice.
		entries := len(r.store.auditActions())

		_, err = r.endLeaseID("bob", lease)
		require.NoError(t, err)
		assert.Equal(t, "disk is dying; lease ended by operator", r.store.host("a").HoldReason)
		assert.Len(t, r.store.auditActions(), entries)
	})

	t.Run("a hold without a reason gets the cancel as its reason", func(t *testing.T) {
		r, lease := mixedFleet(t, false)

		_, err := r.svc.Hold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}})
		require.NoError(t, err)

		_, err = r.endLeaseID("alice", lease)
		require.NoError(t, err)
		assert.Equal(t, "lease ended by operator", r.store.host("a").HoldReason)
	})

	t.Run("what cannot be ended", func(t *testing.T) {
		r, lease := mixedFleet(t, false)

		_, err := r.svc.EndLease(r.ctx, "alice", EndLeaseRequest{})
		require.ErrorIs(t, err, ErrInvalidAction)

		_, err = r.svc.EndLease(r.ctx, "alice", EndLeaseRequest{Lease: "nosuchlease"})
		require.ErrorIs(t, err, ErrUnknownLease)

		r.reportOp("a", lease, protocol.PhaseFailed, true, protocol.StepFailed, protocol.StepPending)
		r.pass()

		_, err = r.endLeaseID("alice", lease)
		require.ErrorIs(t, err, ErrInvalidAction, "a lease that has ended")
		assert.Contains(t, err.Error(), "is failed")
		assert.False(t, r.store.host("a").HoldWanted, "a refused action holds nobody")
	})

	t.Run("a write that fails leaves neither the mark nor the hold", func(t *testing.T) {
		r, lease := mixedFleet(t, false)

		r.store.setFailing(false, 1)

		_, err := r.endLeaseID("alice", lease)
		require.Error(t, err)
		assert.Nil(t, r.storedLease(lease).Cancelled)
		assert.False(t, r.store.host("a").HoldWanted)
	})
}

// A cancel changes nothing about the ways a lease ends without its agent: an
// agent that goes silent fails it, and that breaks the host as it always did.
// And a step that really failed breaks the host for its group, cancelled or
// not; a step that was never begun does not.
func TestReview6CancelledLeaseThatFailsForAReason(t *testing.T) {
	t.Run("the agent goes silent", func(t *testing.T) {
		r, lease := mixedFleet(t, false)

		_, err := r.endLeaseID("alice", lease)
		require.NoError(t, err)

		// Only b and c sync from here on.
		for range 40 {
			r.now = r.now.Add(time.Minute)
			r.refreshHealth()
			r.sync("b", nil)
			r.sync("c", nil)
			r.pass()
		}

		l := r.storedLease(lease)
		assert.Equal(t, fleet.LeaseFailed, l.State)
		assert.Equal(t, "agent silent", l.Error)

		ld := newLedger([]*fleet.Lease{l})
		assert.NotNil(t, ld.brokenSince("a", groupPackages))
		assert.NotNil(t, ld.brokenSince("a", prefixImageKey+imgLighthouse))
	})

	t.Run("a step failed before the cancel took effect", func(t *testing.T) {
		r, lease := mixedFleet(t, false)

		_, err := r.endLeaseID("alice", lease)
		require.NoError(t, err)

		// The image step failed; the packages were never begun. The agent
		// reports the failure, and the failure is the lease's reason.
		r.reportOp("a", lease, protocol.PhaseFailed, true, protocol.StepFailed, protocol.StepCancelled)
		r.pass()

		l := r.storedLease(lease)
		assert.Equal(t, fleet.LeaseFailed, l.State)
		assert.Equal(t, "container did not start", l.Error)

		ld := newLedger([]*fleet.Lease{l})
		assert.NotNil(t, ld.brokenSince("a", prefixImageKey+imgLighthouse), "the step that failed breaks")
		assert.Nil(t, ld.brokenSince("a", groupPackages), "the step that was never begun does not")
	})
}

// The table under "Broken hosts": what the end of a lease says about its
// host for a group.
func TestReview6VerdictTable(t *testing.T) {
	image, packages := imageNeed("c", imgLighthouse, digestA), packagesNeed("2.0")
	imageGroup := prefixImageKey + imgLighthouse
	cancel := func(state fleet.LeaseState) *fleet.LeaseCancel {
		return &fleet.LeaseCancel{By: "alice", State: state}
	}

	tests := []struct {
		name      string
		state     fleet.LeaseState
		reason    string
		cancelled *fleet.LeaseCancel
		// anyCancel: the lease was cancelled, and the row holds whatever
		// state the mark names.
		anyCancel bool
		steps     [2]protocol.StepStatus
		noReport  bool
		// want: the verdict for the image and for the packages.
		want [2]leaseVerdict
	}{
		{
			name: "released, all done", state: fleet.LeaseReleased,
			steps: [2]protocol.StepStatus{protocol.StepDone, protocol.StepDone},
			want:  [2]leaseVerdict{verdictRepaired, verdictRepaired},
		},
		{
			name: "released, one superseded", state: fleet.LeaseReleased,
			steps: [2]protocol.StepStatus{protocol.StepDone, protocol.StepSuperseded},
			want:  [2]leaseVerdict{verdictRepaired, verdictNone},
		},
		{
			name: "failed, one done and the other failed", state: fleet.LeaseFailed, reason: "boom",
			steps: [2]protocol.StepStatus{protocol.StepDone, protocol.StepFailed},
			want:  [2]leaseVerdict{verdictNone, verdictBroke},
		},
		{
			name: "failed while verifying, all done", state: fleet.LeaseFailed, reason: "not ready after 30m0s",
			steps: [2]protocol.StepStatus{protocol.StepDone, protocol.StepDone},
			want:  [2]leaseVerdict{verdictBroke, verdictBroke},
		},
		{
			name: "failed, agent silent", state: fleet.LeaseFailed, reason: reasonAgentSilent, noReport: true,
			want: [2]leaseVerdict{verdictBroke, verdictBroke},
		},
		{
			name: "expired", state: fleet.LeaseExpired,
			steps: [2]protocol.StepStatus{protocol.StepRefused, protocol.StepRefused},
			want:  [2]leaseVerdict{verdictNone, verdictNone},
		},
		// Ended by a person, with the agent's final report on record. The
		// report decides, whatever state the lease was in at the click (each
		// of these rows runs with every state on the mark).
		{
			name: "ended by operator, one done and the other never begun", state: fleet.LeaseFailed,
			reason: reasonEndedByOperator, anyCancel: true,
			steps: [2]protocol.StepStatus{protocol.StepDone, protocol.StepCancelled},
			want:  [2]leaseVerdict{verdictBroke, verdictNone},
		},
		{
			name:  "ended by operator, all done (the cancel came too late, an agent that does not know it, verifying)",
			state: fleet.LeaseFailed, reason: reasonEndedByOperator, anyCancel: true,
			steps: [2]protocol.StepStatus{protocol.StepDone, protocol.StepDone},
			want:  [2]leaseVerdict{verdictBroke, verdictBroke},
		},
		{
			name: "ended by operator, nothing begun", state: fleet.LeaseFailed,
			reason: reasonEndedByOperator, anyCancel: true,
			steps: [2]protocol.StepStatus{protocol.StepCancelled, protocol.StepCancelled},
			want:  [2]leaseVerdict{verdictNone, verdictNone},
		},
		{
			name: "ended by operator, refused and superseded", state: fleet.LeaseFailed,
			reason: reasonEndedByOperator, anyCancel: true,
			steps: [2]protocol.StepStatus{protocol.StepRefused, protocol.StepSuperseded},
			want:  [2]leaseVerdict{verdictNone, verdictNone},
		},
		{
			name: "ended by operator, but a step is reported failed", state: fleet.LeaseFailed,
			reason: reasonEndedByOperator, anyCancel: true,
			steps: [2]protocol.StepStatus{protocol.StepFailed, protocol.StepCancelled},
			want:  [2]leaseVerdict{verdictBroke, verdictNone},
		},
		{
			name: "ended by operator, one done and the other failed", state: fleet.LeaseFailed,
			reason: reasonEndedByOperator, anyCancel: true,
			steps: [2]protocol.StepStatus{protocol.StepDone, protocol.StepFailed},
			want:  [2]leaseVerdict{verdictNone, verdictBroke},
		},
		{
			name:  "cancelled, then the agent's own failure: one failed, one never begun",
			state: fleet.LeaseFailed, reason: "boom", cancelled: cancel(fleet.LeaseRunning),
			steps: [2]protocol.StepStatus{protocol.StepFailed, protocol.StepCancelled},
			want:  [2]leaseVerdict{verdictBroke, verdictNone},
		},
		{
			name:  "cancelled, then the containers did not come back: done is in doubt, never begun is not",
			state: fleet.LeaseFailed, reason: "containers did not stay up", cancelled: cancel(fleet.LeaseRunning),
			steps: [2]protocol.StepStatus{protocol.StepDone, protocol.StepCancelled},
			want:  [2]leaseVerdict{verdictBroke, verdictNone},
		},
		{
			name: "cancelled, then silent", state: fleet.LeaseFailed, reason: reasonAgentSilent,
			cancelled: cancel(fleet.LeaseRunning),
			steps:     [2]protocol.StepStatus{protocol.StepRunning, protocol.StepPending},
			want:      [2]leaseVerdict{verdictBroke, verdictBroke},
		},
		{
			name: "not cancelled: a status cancelled alone spares nobody", state: fleet.LeaseFailed, reason: "boom",
			steps: [2]protocol.StepStatus{protocol.StepCancelled, protocol.StepCancelled},
			want:  [2]leaseVerdict{verdictBroke, verdictBroke},
		},
	}

	for _, tt := range tests {
		marks := []*fleet.LeaseCancel{tt.cancelled}
		if tt.anyCancel {
			marks = []*fleet.LeaseCancel{
				cancel(fleet.LeaseGranted), cancel(fleet.LeaseRunning), cancel(fleet.LeaseVerifying),
			}
		}

		for _, mark := range marks {
			name := tt.name
			if mark != nil && tt.anyCancel {
				name += ", ended while " + string(mark.State)
			}

			t.Run(name, func(t *testing.T) {
				l := &fleet.Lease{
					ID: "l", Host: "a", State: tt.state, Error: tt.reason, Cancelled: mark,
					Needs: []protocol.Need{image, packages},
				}

				if !tt.noReport {
					l.Operation = &protocol.Operation{LeaseID: "l", Steps: []protocol.Step{
						{NeedID: image.ID, Status: tt.steps[0]}, {NeedID: packages.ID, Status: tt.steps[1]},
					}}
				}

				assert.Equal(t, tt.want[0], verdictOf(l, imageGroup), "image")
				assert.Equal(t, tt.want[1], verdictOf(l, groupPackages), "packages")
			})
		}
	}
}

// What adoptOperations ignores. A lease that failed because its agent was
// silent ended without the agent's word. When the agent is back and reports
// the operation, the host is down (or was) under a lease the controller has
// closed: before, the report was ignored, the host did not count against the
// budget while it was down, nobody looked whether it came back, and it stayed
// broken although the operation succeeded. Now the lease is taken up again.
func TestReview6ReportForALeaseThatFailedAsSilent(t *testing.T) {
	play := func(t *testing.T, restart bool) *replay {
		t.Helper()

		r := newReplay(t, restart)

		for _, name := range []string{"a", "b", "c"} {
			r.addHost(name, 0)
		}

		r.reports("2.0", "a", "b", "c")
		r.pass()

		lease := r.running("a")
		rollout := r.openRollout(packagesKey("2.0")).ID

		r.pass()

		// a goes silent for longer than leaseTtl.
		for range 35 {
			r.now = r.now.Add(time.Minute)
			r.refreshHealth()
			r.sync("b", nil)
			r.sync("c", nil)
			r.pass()
		}

		require.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)
		require.Equal(t, "agent silent", r.storedLease(lease).Error)
		require.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State)
		require.Empty(t, r.leased())

		// The sync the lease failed on still carries the operation. That is
		// not the agent being back.
		r.pass()
		require.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)

		// The agent is back, still at work.
		r.advance(time.Minute)
		r.pass()

		l := r.storedLease(lease)
		assert.Equal(t, fleet.LeaseRunning, l.State, "the host is down: it holds its lease again")
		assert.Nil(t, l.EndedAt)
		assert.Empty(t, l.Error)
		assert.Equal(t, []string{"a"}, r.leased())
		assert.Equal(t, 1, r.svc.Plan().Budget.LeasedHosts)
		assert.Equal(t, "failed", r.store.lastAuditOf("lease.reopened").Detail["was"])
		assert.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State, "what halted stays halted until a person retries")

		// It finishes and passes readiness: the lease that failed as silent is
		// released, and the host is in order.
		r.done("a")
		assert.Equal(t, fleet.LeaseReleased, r.storedLease(lease).State)

		r.retry(rollout)
		r.pass()

		m := r.rollout(rollout).Host("a")
		assert.Equal(t, fleet.HostDone, m.State, "the need is gone")
		assert.NotEqual(t, fleet.StageRepair, m.Stage)
		assert.Empty(t, m.Error, "not broken: the last word of its lease is that it was released")
		assert.Len(t, r.leased(), 1)
		assert.NotContains(t, r.leased(), "a")

		return r
	}

	play(t, false)
	sameAfterRestarts(t, play)
}

// What is still ignored: the report for a lease that ended with the agent's
// final word on record. The agent repeats it until it sees a response without
// the lease; it says nothing new.
func TestReview6ReportOnRecordIsNotTakenUpAgain(t *testing.T) {
	need := packagesNeed("2.0")
	ended := func(b *worldBuilder, state fleet.LeaseState, record *protocol.Operation) *fleet.Lease {
		at := b.w.Now.Add(-time.Minute)

		return &fleet.Lease{
			ID: "l-old", Host: "a", State: state, Needs: []protocol.Need{need},
			Rollouts: map[string]string{need.ID: "id000"}, GrantedAt: at.Add(-time.Minute), EndedAt: &at,
			Operation: record,
		}
	}
	report := func(phase protocol.Phase, status protocol.StepStatus) *protocol.Operation {
		return &protocol.Operation{
			LeaseID: "l-old", Phase: phase, Disrupted: true,
			Steps: []protocol.Step{{NeedID: need.ID, Status: status}},
		}
	}

	tests := []struct {
		name string
		// closed is the lease as it ended; now is what the agent reports.
		state     fleet.LeaseState
		record    *protocol.Operation
		now       *protocol.Operation
		otherHost bool
		stale     bool
		want      fleet.LeaseState
	}{
		{
			name: "failed on the agent's failure: the report is on record", state: fleet.LeaseFailed,
			record: report(protocol.PhaseFailed, protocol.StepFailed),
			now:    report(protocol.PhaseFailed, protocol.StepFailed),
		},
		{
			name: "failed on readiness: the done report is on record", state: fleet.LeaseFailed,
			record: report(protocol.PhaseDone, protocol.StepDone),
			now:    report(protocol.PhaseDone, protocol.StepDone),
		},
		{
			name: "released", state: fleet.LeaseReleased,
			record: report(protocol.PhaseDone, protocol.StepDone),
			now:    report(protocol.PhaseDone, protocol.StepDone),
		},
		{
			name: "failed as silent, and the sync is the one from before", state: fleet.LeaseFailed,
			record: report(protocol.PhaseApplying, protocol.StepRunning),
			now:    report(protocol.PhaseApplying, protocol.StepRunning), stale: true,
		},
		{
			name: "the lease of another host", state: fleet.LeaseFailed,
			record: report(protocol.PhaseApplying, protocol.StepRunning),
			now:    report(protocol.PhaseApplying, protocol.StepRunning), otherHost: true,
		},
		{
			name: "failed as silent, the agent is back at work: taken up again", state: fleet.LeaseFailed,
			record: report(protocol.PhaseApplying, protocol.StepRunning),
			now:    report(protocol.PhaseStarting, protocol.StepDone), want: fleet.LeaseRunning,
		},
		{
			name: "failed without any report, the agent is back and done: verified", state: fleet.LeaseFailed,
			now: report(protocol.PhaseDone, protocol.StepDone), want: fleet.LeaseVerifying,
		},
		{
			name: "failed as silent, the agent is back with a failure: failed, with its word", state: fleet.LeaseFailed,
			record: report(protocol.PhaseApplying, protocol.StepRunning),
			now:    report(protocol.PhaseFailed, protocol.StepFailed), want: fleet.LeaseFailed,
		},
		{
			name: "expired, and the agent stopped something after all: taken up again", state: fleet.LeaseExpired,
			now: report(protocol.PhaseApplying, protocol.StepRunning), want: fleet.LeaseRunning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.w.Settings.Paused = true
			b.host("a", 10)
			b.host("b", 10)

			closed := ended(b, tt.state, tt.record)
			if tt.otherHost {
				closed.Host = "b"
			}

			if tt.stale {
				b.get("a").LastSeen = closed.EndedAt.Add(-time.Second)
			}

			b.w.Ended = []*fleet.Lease{closed}
			b.w.ClosedLeases = map[string]*fleet.Lease{closed.ID: closed}
			b.get("a").Status.Operation = tt.now

			out := b.step()

			if tt.want == "" {
				assert.Empty(t, out.Leases, "nothing is taken up again")
				assert.Empty(t, auditActions(out))

				return
			}

			require.Len(t, out.Leases, 1)
			assert.Equal(t, tt.want, out.Leases[0].State)
			assert.Equal(t, "lease.reopened", auditActions(out)[0])
			assert.Equal(t, tt.now, out.Leases[0].Operation)

			if tt.want == fleet.LeaseFailed {
				assert.Equal(t, reasonAgentFailed, out.Leases[0].Error, "the agent's word, not the silence")
			}
		})
	}
}

// L1: the last lease is the one that ended last. Neither the ID (random in
// production) nor the grant time says which that is.
func TestReview6TheLastLeaseIsTheOneThatEndedLast(t *testing.T) {
	need := packagesNeed("2.0")
	base := testGenesis.Add(time.Hour)
	lease := func(id string, state fleet.LeaseState, granted, ended time.Duration) *fleet.Lease {
		at := base.Add(ended)
		status := protocol.StepDone

		if state == fleet.LeaseFailed {
			status = protocol.StepFailed
		}

		return &fleet.Lease{
			ID: id, Host: "a", State: state, Needs: []protocol.Need{need},
			GrantedAt: base.Add(granted), EndedAt: &at,
			Operation: &protocol.Operation{LeaseID: id, Steps: []protocol.Step{{NeedID: need.ID, Status: status}}},
		}
	}

	tests := []struct {
		name   string
		leases []*fleet.Lease
		// broken is the ID of the lease the host is broken since, or "".
		broken string
	}{
		{
			name: "the repair ended last and has the smaller ID",
			leases: []*fleet.Lease{
				lease("zzzz", fleet.LeaseFailed, time.Minute, 2*time.Minute),
				lease("aaaa", fleet.LeaseReleased, 3*time.Minute, 4*time.Minute),
			},
		},
		{
			name: "the failure ended last and has the smaller ID",
			leases: []*fleet.Lease{
				lease("zzzz", fleet.LeaseReleased, time.Minute, 2*time.Minute),
				lease("aaaa", fleet.LeaseFailed, 3*time.Minute, 4*time.Minute),
			},
			broken: "aaaa",
		},
		{
			// A lease that expired, was taken up again and failed in the end:
			// granted first, ended last, with the smaller ID.
			name: "the failure ended last, was granted first and has the smaller ID",
			leases: []*fleet.Lease{
				lease("aaaa", fleet.LeaseFailed, time.Minute, 10*time.Minute),
				lease("zzzz", fleet.LeaseReleased, 5*time.Minute, 6*time.Minute),
			},
			broken: "aaaa",
		},
		{
			name: "the repair ended last, was granted first and has the smaller ID",
			leases: []*fleet.Lease{
				lease("aaaa", fleet.LeaseReleased, time.Minute, 10*time.Minute),
				lease("zzzz", fleet.LeaseFailed, 5*time.Minute, 6*time.Minute),
			},
		},
		{
			name: "three leases, the middle one by ID and by grant ended last",
			leases: []*fleet.Lease{
				lease("aaaa", fleet.LeaseReleased, 9*time.Minute, 10*time.Minute),
				lease("mmmm", fleet.LeaseFailed, 5*time.Minute, 20*time.Minute),
				lease("zzzz", fleet.LeaseReleased, time.Minute, 2*time.Minute),
			},
			broken: "mmmm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// In every order the table may be read in.
			for _, leases := range [][]*fleet.Lease{tt.leases, reversed(tt.leases)} {
				got := ""
				if l := newLedger(leases).brokenSince("a", groupPackages); l != nil {
					got = l.ID
				}

				assert.Equal(t, tt.broken, got)

				// And as the leases end one by one, in the order of their ends.
				byEnd := slices.Clone(leases)
				slices.SortFunc(byEnd, func(x, y *fleet.Lease) int { return x.EndedAt.Compare(*y.EndedAt) })

				ld := make(ledger, 1)
				for _, l := range byEnd {
					ld.note(l)
				}

				got = ""
				if l := ld.brokenSince("a", groupPackages); l != nil {
					got = l.ID
				}

				assert.Equal(t, tt.broken, got)
			}
		})
	}

	// A lease that is taken up again says nothing while it is open: what the
	// lease before it said holds again. When it ends again, its last end
	// counts.
	t.Run("a lease that is taken up again", func(t *testing.T) {
		first := lease("zzzz", fleet.LeaseFailed, time.Minute, 2*time.Minute)
		second := lease("aaaa", fleet.LeaseFailed, 3*time.Minute, 4*time.Minute)

		ld := newLedger([]*fleet.Lease{second, first})
		require.Equal(t, "aaaa", ld.brokenSince("a", groupPackages).ID)

		ld.retract(second)
		require.Equal(t, "zzzz", ld.brokenSince("a", groupPackages).ID, "the lease before it holds again")

		// It ends again and says nothing (its step was superseded): the lease
		// before it still holds.
		nothing := lease("aaaa", fleet.LeaseReleased, 3*time.Minute, 8*time.Minute)
		nothing.Operation.Steps[0].Status = protocol.StepSuperseded
		ld.note(nothing)
		require.Equal(t, "zzzz", ld.brokenSince("a", groupPackages).ID)

		// Or it ends again as released with its step done: repaired.
		ld.note(lease("aaaa", fleet.LeaseReleased, 3*time.Minute, 8*time.Minute))
		assert.Nil(t, ld.brokenSince("a", groupPackages))

		// The digest of the table after a restart says the same.
		assert.Nil(t, newLedger([]*fleet.Lease{first, lease("aaaa", fleet.LeaseReleased, 3*time.Minute, 8*time.Minute)}).
			brokenSince("a", groupPackages))
		assert.Equal(t, "zzzz", newLedger([]*fleet.Lease{first, nothing}).brokenSince("a", groupPackages).ID)
	})
}

func reversed(in []*fleet.Lease) []*fleet.Lease {
	out := slices.Clone(in)
	slices.Reverse(out)

	return out
}

// L4: a host that is forgotten and returns under its name is a new host. The
// leases of the host before it say nothing about it, with and without a
// restart in between: they ended before this database first saw the host.
func TestReview6ForgettingAHostForgetsThatItWasBroken(t *testing.T) {
	// broken: a's lease for the packages of 2.0 failed.
	broken := func(t *testing.T, restart bool) (r *replay, lease string) {
		t.Helper()

		r = newReplay(t, restart)

		for _, name := range []string{"a", "b", "c"} {
			r.addHost(name, 0)
		}

		r.reports("2.0", "a", "b", "c")
		r.pass()
		require.Equal(t, []string{"a"}, r.leased())

		lease = r.running("a")

		r.pass()
		r.fail("a")
		r.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
		r.pass()
		require.Equal(t, fleet.RolloutHalted, r.openRollout(packagesKey("2.0")).State)
		require.Empty(t, r.leased())

		return r, lease
	}

	// The control: the host stays, newer packages appear, and it goes first.
	t.Run("a host that stays is broken", func(t *testing.T) {
		r, lease := broken(t, false)

		r.advance(time.Minute)
		r.reports("3.0", "a", "b", "c")
		r.pass()

		next := r.openRollout(packagesKey("3.0"))
		assert.Equal(t, []string{"a"}, repairStage(next))
		assert.NotEqual(t, lease, next.Host("a").LeaseID, "served anew")
		assert.Equal(t, []string{"a"}, r.leased())
	})

	play := func(t *testing.T, restart bool) *replay {
		t.Helper()

		r, lease := broken(t, restart)
		r.fresh()

		_, err := r.svc.Forget(r.ctx, "alice", HoldRequest{Hosts: []string{"a"}, Reason: "replaced"})
		require.NoError(t, err)
		r.dump()
		r.pass()

		// A new machine with the old name, and newer packages for everybody.
		r.advance(time.Minute)

		host := r.addHost("a", 0)
		host.FirstSeen = r.now
		r.store.putHost(host)
		r.reports("3.0", "a", "b", "c")
		r.pass()

		next := r.openRollout(packagesKey("3.0"))
		require.NotNil(t, next.Host("a"))
		assert.Empty(t, repairStage(next), "the new host is not broken")
		assert.NotContains(t, next.Host("a").Error, brokenPrefix)

		for host, reason := range queueReasons(r.svc.Plan()) {
			assert.NotEqual(t, "after a", reason, host)
		}

		// The lease of the host before it is still in the table.
		assert.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)

		return r
	}

	t.Run("a host that returns is not, without a restart", func(t *testing.T) { play(t, false) })
	t.Run("nor with restarts", func(t *testing.T) { play(t, true) })
	sameAfterRestarts(t, play)

	// The rule itself: a lease that ended before the host was first seen says
	// nothing; one that ended at that moment or after it does.
	for name, tt := range map[string]struct {
		ended  time.Duration
		broken bool
	}{
		"ended before the host was first seen": {ended: -time.Millisecond},
		"ended when the host was first seen":   {ended: 0, broken: true},
		"ended after it":                       {ended: time.Minute, broken: true},
	} {
		t.Run(name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.w.Settings.Paused = true
			b.host("a", 0)
			b.host("b", 0)
			b.wants("a", packagesNeed("3.0"))
			b.wants("b", packagesNeed("3.0"))

			seen := b.w.Now.Add(-time.Hour)
			b.get("a").FirstSeen = seen

			l := b.broke("a", packagesNeed("2.0"))
			at := seen.Add(tt.ended)
			l.EndedAt = &at

			b.step()

			m := b.mustRollout(packagesKey("3.0")).Host("a")
			assert.Equal(t, tt.broken, m.Stage == fleet.StageRepair)
		})
	}
}

// L5: a beacon node that no longer answers is reported as unreachable, also
// when it published a timing error while it still answered. Before, the
// timing error stood for ever.
func TestReview6GateReasonOfAStaleViewWithATimingError(t *testing.T) {
	const reason = "phase 0 (from epoch 0) has slots of 3600000 ms and epochs of 32 slots"

	gateOf := func(t *testing.T, mod func(n *fleet.NetworkHealth)) []string {
		t.Helper()

		b := newWorldBuilder(t)
		b.healthMod = mod
		b.refreshHealth()
		b.host("a", 32)
		b.wants("a", packagesNeed("2.0"))

		out := b.step()
		require.False(t, out.Plan.Gate.OK)
		require.Empty(t, b.leasedHosts())

		return out.Plan.Gate.Reasons
	}

	t.Run("the node answers and its timing is out of bounds", func(t *testing.T) {
		reasons := gateOf(t, func(n *fleet.NetworkHealth) {
			n.Phases = fleet.SinglePhase(testGenesis, time.Hour)
			n.TimingError = reason
		})

		assert.Equal(t, gateTimingPrefix+reason, reasons[0])
		assert.NotContains(t, reasons, gateBeacon)
	})

	t.Run("no phase can be worked with and the view is old", func(t *testing.T) {
		reasons := gateOf(t, func(n *fleet.NetworkHealth) {
			n.Phases = fleet.SinglePhase(testGenesis, time.Hour)
			n.TimingError = reason
			n.UpdatedAt = n.UpdatedAt.Add(-2*fallbackEpoch - time.Second)
		})

		assert.Equal(t, gateBeacon, reasons[0])
		assert.NotContains(t, reasons, gateTimingPrefix+reason)
	})

	t.Run("no phase can be worked with and the view is two fallback epochs old: not yet", func(t *testing.T) {
		reasons := gateOf(t, func(n *fleet.NetworkHealth) {
			n.Phases = fleet.SinglePhase(testGenesis, time.Hour)
			n.TimingError = reason
			n.UpdatedAt = n.UpdatedAt.Add(-2 * fallbackEpoch)
		})

		assert.Equal(t, gateTimingPrefix+reason, reasons[0])
	})

	// The chain has reached a phase that is out of bounds; the phase before
	// it measures how old the view may be.
	t.Run("the last good phase measures the age", func(t *testing.T) {
		broken := func(n *fleet.NetworkHealth, age time.Duration) {
			start, ok := n.EpochStart(n.CurrentEpoch)
			require.True(t, ok)

			n.Phases = append(slices.Clone(n.Phases),
				fleet.SlotPhase{Epoch: n.CurrentEpoch, Start: start, SlotMs: 2 * fleet.MaxSlotMs})
			n.TimingError = reason
			n.UpdatedAt = n.UpdatedAt.Add(-age)

			require.Zero(t, n.EpochDuration())
			require.Equal(t, testEpoch, n.LastEpochDuration())
		}

		fresh := gateOf(t, func(n *fleet.NetworkHealth) { broken(n, 2*testEpoch) })
		assert.Equal(t, gateTimingPrefix+reason, fresh[0])

		old := gateOf(t, func(n *fleet.NetworkHealth) { broken(n, 2*testEpoch+time.Second) })
		assert.Equal(t, gateBeacon, old[0])
	})

	t.Run("a good timing: two epochs, as before", func(t *testing.T) {
		reasons := gateOf(t, func(n *fleet.NetworkHealth) { n.UpdatedAt = n.UpdatedAt.Add(-2*testEpoch - time.Second) })
		assert.Equal(t, gateBeacon, reasons[0])
	})
}

// Found by the simulation: a lease that a person ended before the controller
// knew of an operation is withheld from the agent. An agent that has started
// it all the same reports the operation, and when it reports it as over while
// the scheduler has not taken the report in yet (a reconcile that failed, or
// has not run), a response without the lease made it forget the operation:
// the lease then expired as never started although the host had been down.
// The lease is restated, with the mark, to an agent that reports an operation
// for it.
func TestReview6AnAgentThatReportsTheLeaseIsToldOfIt(t *testing.T) {
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
	assert.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)
	assert.Equal(t, "ended by operator", r.storedLease(lease).Error)
}

// Found by the simulation: an agent whose operation is over is free to start
// the lease it was granted since. That lease is not expired in favour of the
// one the agent reports as finished.
func TestReview6AFinishedOperationDoesNotDisplaceAGrantedLease(t *testing.T) {
	need := packagesNeed("2.0")

	for name, tt := range map[string]struct {
		phase     protocol.Phase
		displaced bool
	}{
		"the agent is at work on the old lease: the new one is expired in its favour": {
			phase: protocol.PhaseApplying, displaced: true,
		},
		"the old operation is over: the new lease stays": {phase: protocol.PhaseDone},
	} {
		t.Run(name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.host("a", 0)
			b.host("b", 0)
			b.wants("a", packagesNeed("3.0"))
			b.step()
			require.Equal(t, []string{"a"}, b.leasedHosts())

			granted := b.lease("a").ID
			at := b.w.Now.Add(-time.Minute)
			old := &fleet.Lease{
				ID: "l-old", Host: "a", State: fleet.LeaseFailed, Error: reasonAgentSilent,
				Needs: []protocol.Need{need}, GrantedAt: at.Add(-time.Minute), EndedAt: &at,
			}
			b.w.Ended = append(b.w.Ended, old)
			b.w.ClosedLeases = map[string]*fleet.Lease{old.ID: old}
			b.get("a").Status.Operation = &protocol.Operation{
				LeaseID: old.ID, Phase: tt.phase, Disrupted: true,
				Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepDone}},
			}

			out := b.step()

			if !tt.displaced {
				assert.Empty(t, auditActions(out))
				assert.Equal(t, granted, b.lease("a").ID)

				return
			}

			assert.Equal(t, []string{"lease.expired", "lease.reopened"}, auditActions(out))
			assert.Equal(t, old.ID, b.lease("a").ID)
		})
	}
}

// L7: a lease with two steps of one group, one done and one failed, breaks
// the host for the group: done means every step of the group.
func TestReview6OneStepOfAGroupDoneIsNotTheGroupDone(t *testing.T) {
	first, second := imageNeed("c0", imgLighthouse, digestA), imageNeed("c1", imgLighthouse, digestA)
	other := packagesNeed("2.0")
	group := prefixImageKey + imgLighthouse

	lease := func(state fleet.LeaseState, statuses ...protocol.StepStatus) *fleet.Lease {
		l := &fleet.Lease{
			ID: "l", Host: "a", State: state, Needs: []protocol.Need{first, second, other},
			Operation: &protocol.Operation{LeaseID: "l"},
		}

		for i, n := range l.Needs {
			l.Operation.Steps = append(l.Operation.Steps, protocol.Step{NeedID: n.ID, Status: statuses[i]})
		}

		return l
	}

	done, failed, superseded := protocol.StepDone, protocol.StepFailed, protocol.StepSuperseded

	assert.Equal(t, verdictBroke, verdictOf(lease(fleet.LeaseFailed, done, failed, done), group))
	assert.Equal(t, verdictNone, verdictOf(lease(fleet.LeaseFailed, done, failed, done), groupPackages))
	assert.Equal(t, verdictBroke, verdictOf(lease(fleet.LeaseFailed, done, superseded, failed), group),
		"not all done: in doubt")
	assert.Equal(t, verdictNone, verdictOf(lease(fleet.LeaseFailed, done, done, failed), group))
	assert.Equal(t, verdictNone, verdictOf(lease(fleet.LeaseReleased, done, superseded, done), group),
		"released, but not every step of the group done: no repair")
	assert.Equal(t, verdictRepaired, verdictOf(lease(fleet.LeaseReleased, done, done, superseded), group))
}

// A lease that is taken up again leases its member again where the member is
// still failed by it: a person had reopened the rollout without retrying the
// host. When the lease then fails for good, the rollout halts again. Before,
// the member stayed failed in an active rollout that nothing halted.
func TestReview6ALeaseTakenUpAgainLeasesItsFailedMemberAgain(t *testing.T) {
	r := newReplay(t, false)

	for _, name := range []string{"a", "b", "c"} {
		r.addHost(name, 0)
	}

	r.reports("2.0", "a", "b", "c")
	r.pass()

	lease := r.running("a")
	rollout := r.openRollout(packagesKey("2.0")).ID

	r.pass()

	for range 35 {
		r.now = r.now.Add(time.Minute)
		r.refreshHealth()
		r.sync("b", nil)
		r.sync("c", nil)
		r.pass()
	}

	require.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)
	require.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State)

	// A retry that names only b no longer reopens the rollout: b did not
	// fail, so nothing changes.
	_, err := r.svc.Retry(r.ctx, "alice", RolloutHostsRequest{Rollout: rollout, Hosts: []string{"b"}})
	require.ErrorIs(t, err, ErrInvalidAction)
	require.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State)

	// An active rollout with a failed member is what an earlier build left
	// behind after such a retry. The row is written as that build wrote it.
	left := r.rollout(rollout)
	left.State = fleet.RolloutActive
	left.Reason = ""
	require.NoError(t, r.store.Commit(r.ctx, &store.Changes{Rollouts: []*fleet.Rollout{left}}))
	r.svc.hist = history{}

	require.Equal(t, fleet.RolloutActive, r.rollout(rollout).State)
	require.Equal(t, fleet.HostFailed, r.rollout(rollout).Host("a").State)

	// The agent of a is back, at work.
	r.sync("a", nil)
	r.pass()

	require.Equal(t, fleet.LeaseRunning, r.storedLease(lease).State)

	m := r.rollout(rollout).Host("a")
	assert.Equal(t, fleet.HostLeased, m.State)
	assert.Equal(t, lease, m.LeaseID)
	assert.Empty(t, m.Error)

	// And fails.
	r.reportOp("a", lease, protocol.PhaseFailed, true, protocol.StepFailed)
	r.pass()

	assert.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)
	assert.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State)
	assert.Equal(t, fleet.HostFailed, r.rollout(rollout).Host("a").State)
}

// While a lease that had failed is open again, what its failure said about
// the host is taken back: the host is not broken for the rollouts that look.
func TestReview6ALeaseTakenUpAgainSaysNothingWhileItIsOpen(t *testing.T) {
	need := packagesNeed("2.0")

	b := newWorldBuilder(t)
	b.w.Settings.Paused = true
	b.host("a", 0)
	b.host("b", 0)
	b.wants("a", packagesNeed("3.0"))
	b.wants("b", packagesNeed("3.0"))

	at := b.w.Now.Add(-time.Minute)
	silent := &fleet.Lease{
		ID: "l-silent", Host: "a", State: fleet.LeaseFailed, Error: reasonAgentSilent,
		Needs: []protocol.Need{need}, Rollouts: map[string]string{need.ID: "id000"},
		GrantedAt: at.Add(-time.Hour), EndedAt: &at,
	}
	b.w.Ended = []*fleet.Lease{silent}
	b.w.ClosedLeases = map[string]*fleet.Lease{silent.ID: silent}

	b.step()
	require.Equal(t, fleet.StageRepair, b.mustRollout(packagesKey("3.0")).Host("a").Stage, "broken by the silence")

	b.get("a").Status.Operation = &protocol.Operation{
		LeaseID: silent.ID, Phase: protocol.PhaseApplying, Disrupted: true,
		Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepRunning}},
	}

	out := b.step()
	require.Contains(t, auditActions(out), "lease.reopened")

	m := b.mustRollout(packagesKey("3.0")).Host("a")
	assert.NotEqual(t, fleet.StageRepair, m.Stage)
	assert.NotContains(t, m.Error, brokenPrefix)
}
