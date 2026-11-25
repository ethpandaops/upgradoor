package scheduler

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// leasedFleet is two hosts with the same packages need, of which "a" holds
// the lease (it is the canary) and "b" waits for it.
func leasedFleet(t *testing.T) (h *harness, lease string) {
	t.Helper()

	h = newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0, packagesNeed("2.0"))
	h.tick()

	offered := h.assignment("a").Lease
	require.NotNil(t, offered)

	return h, offered.ID
}

// operation makes the host's agent report an operation for the lease, every
// step in the status given.
func (h *harness) operation(host, lease string, phase protocol.Phase, disrupted bool, status protocol.StepStatus) {
	h.t.Helper()

	l, err := h.store.GetLease(h.ctx, lease)
	require.NoError(h.t, err)

	h.sync(host, func(st *protocol.SyncRequest) {
		op := &protocol.Operation{LeaseID: lease, Phase: phase, Disrupted: disrupted}
		for _, n := range l.Needs {
			op.Steps = append(op.Steps, protocol.Step{NeedID: n.ID, Status: status})
		}

		st.Operation = op
	})
}

func (h *harness) lease(id string) *fleet.Lease {
	h.t.Helper()

	l, err := h.store.GetLease(h.ctx, id)
	require.NoError(h.t, err)

	return l
}

// Ending a lease ends nothing by itself: the lease is marked, keeps its slot
// and is restated to the agent with the mark. It ends as failed when the
// agent's operation is over, and that halts its rollout. No hold is written.
func TestEndLeaseOfARunningLease(t *testing.T) {
	h, lease := leasedFleet(t)
	h.operation("a", lease, protocol.PhaseApplying, true, protocol.StepRunning)
	h.tick()
	require.False(t, h.assignment("a").Lease.Cancel)

	res, err := h.svc.EndLease(h.ctx, "alice", EndLeaseRequest{Lease: lease, Reason: "taken away"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, res.Hosts)
	assert.Equal(t, "lease "+lease+" of a is ending", res.Message)

	l := h.lease(lease)
	assert.Equal(t, fleet.LeaseRunning, l.State)
	assert.Equal(t, &fleet.LeaseCancel{At: h.now, By: "alice", State: fleet.LeaseRunning, Reason: "taken away"}, l.Cancelled)
	assert.True(t, h.assignment("a").Lease.Cancel)
	assert.False(t, h.store.host("a").Held(), "ending a lease holds nobody")
	assert.Empty(t, h.svc.Plan().Hosts["a"].HoldRequests)

	entry := h.store.lastAuditOf("action.end-lease")
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, "a", entry.Target)
	assert.Equal(t, map[string]any{keyLease: lease, keyState: fleet.LeaseRunning, keyReason: "taken away"}, entry.Detail)

	// While the agent works the host is down, and its slot is taken.
	h.advance(time.Minute)
	h.tick()
	assert.Equal(t, fleet.LeaseRunning, h.lease(lease).State)
	assert.Equal(t, 1, h.svc.Plan().Budget.LeasedHosts)
	assert.True(t, h.assignment("a").Lease.Cancel, "restated on every sync")

	// Ending it again changes nothing.
	again, err := h.svc.EndLease(h.ctx, "bob", EndLeaseRequest{Lease: lease})
	require.NoError(t, err)
	assert.Contains(t, again.Message, "was already ended by alice")
	assert.Equal(t, "alice", h.lease(lease).Cancelled.By)

	// The agent stopped: its report ends the lease, as cancelled. No
	// failure: the rollout pauses, the host is not broken nor shown failed.
	h.operation("a", lease, protocol.PhaseDone, true, protocol.StepCancelled)
	h.tick()

	l = h.lease(lease)
	assert.Equal(t, fleet.LeaseCancelled, l.State)
	assert.Empty(t, l.Error)
	assert.Nil(t, h.assignment("a").Lease)
	assert.Empty(t, h.store.host("a").Broken)
	assert.NotNil(t, h.store.lastAuditOf("lease.cancelled"))

	id := l.Rollouts[packagesNeed("2.0").ID]
	r := h.rollout(id)
	assert.Equal(t, fleet.RolloutActive, r.State)
	assert.True(t, r.Paused)
	assert.Equal(t, "a cancelled by alice", r.PauseReason)
	assert.Equal(t, fleet.HostCancelled, r.Host("a").State)
	assert.NotEqual(t, SlotFailed, h.svc.Plan().Hosts["a"].Slot)

	// Nothing is offered while the rollout is paused.
	h.advance(time.Minute)
	h.tick()
	assert.Nil(t, h.assignment("a").Lease)
	assert.Nil(t, h.assignment("b").Lease)

	// A resume goes on with the others; the cancelled host waits for a retry.
	_, err = h.svc.Resume(h.ctx, "bob", ScopeRequest{Rollout: id})
	require.NoError(t, err)

	for range 3 {
		h.advance(time.Minute)
		h.tick()
	}

	assert.Nil(t, h.assignment("a").Lease)
	assert.NotNil(t, h.assignment("b").Lease)
	assert.Equal(t, fleet.HostCancelled, h.rollout(id).Host("a").State, "a resume is no retry")
	assert.Equal(t, fleet.RolloutActive, h.rollout(id).State, "a cancelled member keeps the rollout open")

	_, err = h.svc.Retry(h.ctx, "bob", RolloutHostsRequest{Rollout: id, Hosts: []string{"a"}})
	require.NoError(t, err)
	assert.Equal(t, fleet.HostPending, h.rollout(id).Host("a").State)

	_, err = h.svc.EndLease(h.ctx, "alice", EndLeaseRequest{Lease: lease})
	require.ErrorIs(t, err, ErrInvalidAction, "a lease that has ended cannot be ended")
}

// A lease that is being verified when it is ended is cancelled in the next
// reconcile: its operation is over.
func TestEndLeaseOfAVerifyingLease(t *testing.T) {
	h, lease := leasedFleet(t)
	h.operation("a", lease, protocol.PhaseDone, true, protocol.StepDone)
	h.tick()
	require.Equal(t, fleet.LeaseVerifying, h.lease(lease).State)

	_, err := h.svc.EndLease(h.ctx, "alice", EndLeaseRequest{Lease: lease})
	require.NoError(t, err)
	assert.Equal(t, fleet.LeaseVerifying, h.lease(lease).State)

	h.tick()
	assert.Equal(t, fleet.LeaseCancelled, h.lease(lease).State)
	assert.Equal(t, fleet.HostCancelled, h.rollout(h.openRollouts()[0].ID).Host("a").State)
}

// A person lets a rollout pass the gate: the flag is stored and audited, and
// setting it again changes nothing.
func TestIgnoreGateAction(t *testing.T) {
	h, _ := leasedFleet(t)
	id := h.openRollouts()[0].ID

	res, err := h.svc.IgnoreGate(h.ctx, "alice", ScopeRequest{Rollout: id, Reason: "beacon nodes down"})
	require.NoError(t, err)
	assert.Equal(t, id, res.Rollout)
	assert.True(t, h.rollout(id).IgnoreGate)

	entry := h.store.lastAuditOf("action.ignore-gate")
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, id, entry.Target)
	assert.Equal(t, map[string]any{
		keyBefore: map[string]any{keyIgnoreGate: false},
		keyAfter:  map[string]any{keyIgnoreGate: true},
		keyReason: "beacon nodes down",
	}, entry.Detail)

	_, err = h.svc.IgnoreGate(h.ctx, "bob", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	assert.Equal(t, 1, h.audited("action.ignore-gate"))

	_, err = h.svc.IgnoreGate(h.ctx, "alice", ScopeRequest{Rollout: "nope"})
	require.ErrorIs(t, err, ErrUnknownRollout)
}

// A person lets a rollout that waits for the budget pass it: the flag is
// stored and audited, and the host that waited is granted.
func TestIgnoreBudgetAction(t *testing.T) {
	h := newHarness(t)
	h.addHost("idle", 80)
	h.addHost("a", 10, packagesNeed("2.0"))
	h.addHost("b", 10, packagesNeed("3.0"))
	h.advance(time.Second)
	h.tick()

	// One host has the floor lease; the other waits for the budget.
	plan := h.svc.Plan()
	require.Len(t, plan.Queue, 1)

	wait := plan.Queue[0]
	require.Equal(t, WaitBudget, wait.Wait)
	require.NotEmpty(t, wait.Rollout)

	res, err := h.svc.IgnoreBudget(h.ctx, "alice", ScopeRequest{Rollout: wait.Rollout, Reason: "fix for the outage"})
	require.NoError(t, err)
	assert.Equal(t, wait.Rollout, res.Rollout)
	assert.True(t, h.rollout(wait.Rollout).IgnoreBudget)

	entry := h.store.lastAuditOf("action.ignore-budget")
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, map[string]any{
		keyBefore: map[string]any{keyIgnoreBudget: false},
		keyAfter:  map[string]any{keyIgnoreBudget: true},
		keyReason: "fix for the outage",
	}, entry.Detail)

	_, err = h.svc.IgnoreBudget(h.ctx, "bob", ScopeRequest{Rollout: wait.Rollout})
	require.NoError(t, err)
	assert.Equal(t, 1, h.audited("action.ignore-budget"))

	h.tick()
	assert.NotNil(t, h.assignment(wait.Host).Lease)

	_, err = h.svc.IgnoreBudget(h.ctx, "alice", ScopeRequest{Rollout: "nope"})
	require.ErrorIs(t, err, ErrUnknownRollout)
}

// While the gate is closed, resume approves a rollout whatever its policy
// (unless it only lifts a pause); approved, it passes the gate one host at a
// time.
func TestResumeApprovesAtAClosedGate(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0, packagesNeed("2.0"))
	h.health.FinalityLagEpochs = 5
	h.tick()

	id := h.openRollouts()[0].ID
	assert.Equal(t, WaitGate, h.svc.Plan().Queue[0].Wait)
	assert.Equal(t, id, h.svc.Plan().Queue[0].Rollout)

	_, err := h.svc.Pause(h.ctx, "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)

	res, err := h.svc.Resume(h.ctx, "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	assert.Equal(t, "rollout resumed", res.Message)
	assert.False(t, h.rollout(id).Approved, "lifting a pause approves nothing")

	res, err = h.svc.Resume(h.ctx, "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	assert.Equal(t, "rollout approved", res.Message)
	assert.True(t, h.rollout(id).Approved)

	h.tick()
	assert.Len(t, h.leased(), 1, "one host at a time")
}

// A person approves a verifying lease: it is released in the next reconcile
// without waiting for readiness, and its host counts as the canary's success.
// Only a verifying lease of a member can be approved.
func TestApproveLease(t *testing.T) {
	h, lease := leasedFleet(t)
	id := h.openRollouts()[0].ID

	_, err := h.svc.ApproveLease(h.ctx, "alice", ApproveLeaseRequest{Rollout: id, Host: "a"})
	require.ErrorIs(t, err, ErrInvalidAction, "a granted lease is not verifying")
	_, err = h.svc.ApproveLease(h.ctx, "alice", ApproveLeaseRequest{Rollout: id, Host: "b"})
	require.ErrorIs(t, err, ErrInvalidAction, "b holds no lease")
	_, err = h.svc.ApproveLease(h.ctx, "alice", ApproveLeaseRequest{Rollout: id, Host: "c"})
	require.ErrorIs(t, err, ErrUnknownHost)

	h.operation("a", lease, protocol.PhaseDone, true, protocol.StepDone)
	h.tick()
	require.Equal(t, fleet.LeaseVerifying, h.lease(lease).State)

	res, err := h.svc.ApproveLease(h.ctx, "alice", ApproveLeaseRequest{Rollout: id, Host: "a", Reason: "no liveness"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, res.Hosts)

	l := h.lease(lease)
	assert.Equal(t, fleet.LeaseVerifying, l.State, "released by the reconcile")
	assert.Equal(t, &fleet.LeaseApproval{At: h.now, By: "alice", Reason: "no liveness"}, l.Approved)

	entry := h.store.lastAuditOf("action.approve-lease")
	assert.Equal(t, "a", entry.Target)
	assert.Equal(t, map[string]any{
		keyLease: lease, keyRollouts: []string{id}, "readiness": l.Readiness, keyReason: "no liveness",
	}, entry.Detail)

	h.tick()
	assert.Equal(t, fleet.LeaseReleased, h.lease(lease).State)
	assert.Equal(t, "alice", h.store.lastAuditOf("lease.released").Detail["approvedBy"])
	assert.Equal(t, fleet.HostDone, h.rollout(id).Host("a").State)

	h.tick()
	assert.NotNil(t, h.assignment("b").Lease, "the canary succeeded")
}

// A retried host whose ended lease did its step and whose need is gone is
// not done by that: a recheck lease, which is not offered to the agent,
// repeats the readiness check.
func TestRetryAfterAnEndedLeaseRechecksReadiness(t *testing.T) {
	h, lease := leasedFleet(t)
	id := h.openRollouts()[0].ID

	h.operation("a", lease, protocol.PhaseDone, true, protocol.StepDone)
	h.sync("a", func(st *protocol.SyncRequest) { st.Needs = nil })
	h.tick()

	_, err := h.svc.EndLease(h.ctx, "alice", EndLeaseRequest{Lease: lease})
	require.NoError(t, err)
	h.tick()
	require.Equal(t, fleet.HostCancelled, h.rollout(id).Host("a").State)
	require.True(t, h.rollout(id).Paused)
	h.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })

	_, err = h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"a"}})
	require.NoError(t, err)

	assert.False(t, h.rollout(id).Paused, "the pause of the cancel ends with the retry")

	m := h.rollout(id).Host("a")
	require.Equal(t, fleet.HostLeased, m.State, "not done without the readiness check")

	recheck := h.lease(m.LeaseID)
	assert.True(t, recheck.Recheck)
	assert.Equal(t, fleet.LeaseVerifying, recheck.State)
	assert.Equal(t, h.lease(lease).Needs, recheck.Needs)
	require.NotNil(t, h.lease(lease).Baseline)
	assert.Equal(t, h.lease(lease).Baseline, recheck.Baseline, "judged against the host as it was before")
	assert.Nil(t, h.assignment("a").Lease, "the agent is not offered a recheck")
	assert.Equal(t, true, h.store.lastAuditOf("lease.granted").Detail["recheck"])

	h.tick()
	assert.Equal(t, fleet.HostLeased, h.rollout(id).Host("a").State)

	for left := 2 * testEpoch; left > 0; left -= time.Minute {
		h.advance(min(left, time.Minute))
		h.tick()
	}

	assert.Equal(t, fleet.LeaseReleased, h.lease(recheck.ID).State)
	assert.Equal(t, fleet.HostDone, h.rollout(id).Host("a").State)
	assert.Equal(t, recheck.ID, h.rollout(id).Host("a").LeaseID)
}

// A lease that the agent has not started is no longer offered once it is
// ended. It ends as cancelled, never as expired (an expired lease would hand
// the host the same work at once), or as failed when a step failed.
func TestEndLeaseOfAGrantedLease(t *testing.T) {
	tests := []struct {
		name string
		// then is what happens after the lease was ended.
		then func(h *harness, lease string)
		want fleet.LeaseState
	}{
		{
			name: "the agent declines it",
			then: func(h *harness, lease string) {
				h.operation("a", lease, protocol.PhaseDone, false, protocol.StepRefused)
			},
			want: fleet.LeaseCancelled,
		},
		{
			name: "the agent never had it: its next sync carries no operation",
			then: func(h *harness, _ string) { h.advance(2 * time.Second) },
			want: fleet.LeaseCancelled,
		},
		{
			name: "the agent stopped it",
			then: func(h *harness, lease string) {
				h.operation("a", lease, protocol.PhaseFailed, true, protocol.StepCancelled)
			},
			want: fleet.LeaseCancelled,
		},
		{
			name: "the agent had started it and fails",
			then: func(h *harness, lease string) {
				h.operation("a", lease, protocol.PhaseFailed, true, protocol.StepFailed)
			},
			want: fleet.LeaseFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, lease := leasedFleet(t)

			_, err := h.svc.EndLease(h.ctx, "alice", EndLeaseRequest{Lease: lease})
			require.NoError(t, err)

			a := h.assignment("a")
			assert.Nil(t, a.Lease, "not offered to an agent that has not said it has it")
			assert.Nil(t, a.Queue, "and the host is not queued: it holds a lease")

			h.tick()
			require.Equal(t, fleet.LeaseGranted, h.lease(lease).State, "no sync since it was ended")

			tt.then(h, lease)
			h.tick()

			l := h.lease(lease)
			assert.Equal(t, tt.want, l.State)

			r := h.rollout(l.Rollouts[packagesNeed("2.0").ID])
			if tt.want == fleet.LeaseFailed {
				assert.Equal(t, fleet.RolloutHalted, r.State)
				assert.Equal(t, fleet.HostFailed, r.Host("a").State)

				return
			}

			assert.Equal(t, fleet.RolloutActive, r.State)
			assert.True(t, r.Paused)
			assert.Equal(t, fleet.HostCancelled, r.Host("a").State)
		})
	}
}

func TestEndLeaseNamesAnOpenLease(t *testing.T) {
	h, lease := leasedFleet(t)

	_, err := h.svc.EndLease(h.ctx, "alice", EndLeaseRequest{})
	require.ErrorIs(t, err, ErrInvalidAction)

	_, err = h.svc.EndLease(h.ctx, "alice", EndLeaseRequest{Lease: "nosuchlease"})
	require.ErrorIs(t, err, ErrUnknownLease)

	// A write that fails leaves the lease as it was.
	h.store.failNext = 1

	_, err = h.svc.EndLease(h.ctx, "alice", EndLeaseRequest{Lease: lease})
	require.ErrorIs(t, err, errWrite)
	assert.Nil(t, h.lease(lease).Cancelled)
}

// Hold and unhold are not tied to the end of a lease: a person holds the
// host of an ended lease, or lifts its hold, at any time.
func TestHoldIsIndependentOfAnEndedLease(t *testing.T) {
	h, lease := leasedFleet(t)

	_, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a"}, Reason: "look at it"})
	require.NoError(t, err)
	h.obeyHolds("a")

	_, err = h.svc.EndLease(h.ctx, "alice", EndLeaseRequest{Lease: lease})
	require.NoError(t, err)
	assert.Equal(t, "look at it", h.store.host("a").HoldOf(fleet.HolderStaff).Reason, "the hold is as the person set it")

	_, err = h.svc.Unhold(h.ctx, "alice", HoldRequest{Hosts: []string{"a"}})
	require.NoError(t, err)
	h.obeyHolds("a")
	assert.False(t, h.store.host("a").Held())
	assert.NotNil(t, h.lease(lease).Cancelled)
}

// An abort of a rollout of an observed cause is final: the waiting members
// are aborted, an open lease finishes and settles its member, and the cause
// is not opened again until it changes.
func TestAbortClosesARolloutOfAnObservedCause(t *testing.T) {
	h, lease := leasedFleet(t)
	id := h.openRollouts()[0].ID

	res, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: id, Reason: "bad build"})
	require.NoError(t, err)
	assert.Equal(t, id, res.Rollout)
	assert.Contains(t, res.Message, "rollout aborted")

	r := h.rollout(id)
	assert.Equal(t, fleet.RolloutAborted, r.State)
	assert.Equal(t, "bad build", r.Reason)
	assert.Equal(t, fleet.HostLeased, r.Host("a").State)
	assert.Equal(t, fleet.HostAborted, r.Host("b").State)
	assert.Equal(t, fleet.LeaseGranted, h.lease(lease).State, "open leases finish")

	h.addHost("late", 0, packagesNeed("2.0"))
	h.finish("a")

	assert.Equal(t, fleet.HostDone, h.rollout(id).Host("a").State, "the lease's outcome")
	assert.Empty(t, h.openRollouts(), "the aborted cause is not opened again")

	for _, name := range []string{"b", "late"} {
		assert.Nil(t, h.assignment(name).Lease)
	}

	_, err = h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: id})
	require.ErrorIs(t, err, ErrInvalidAction)
	assert.Contains(t, err.Error(), "final")

	// Another target is another cause.
	h.reports("3.0", "b")
	h.tick()

	require.Len(t, h.openRollouts(), 1)
	assert.NotNil(t, h.assignment("b").Lease)
}

// The reboot rollout's key does not change with its target: aborting it
// pauses it until a resume.
func TestAbortPausesTheRebootRollout(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, rebootNeed())
	h.addHost("b", 0, rebootNeed())
	h.tick()

	id := h.openRollouts()[0].ID

	res, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: id, Reason: "bad kernel"})
	require.NoError(t, err)
	assert.Contains(t, res.Message, "rollout paused")

	r := h.rollout(id)
	assert.Equal(t, fleet.RolloutActive, r.State)
	assert.Equal(t, "aborted: bad kernel; resume to continue", r.PauseReason)
}

// A manual rollout is closed by an abort: its unanswered directives are
// cancelled, and it cannot be retried.
func TestAbortClosesAManualRollout(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0)
	h.addHost("b", 0)

	res, err := h.svc.Reboot(h.ctx, "alice", RebootRequest{All: true, Staggered: true})
	require.NoError(t, err)
	require.Len(t, h.assignment("a").Directives, 1)

	_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{})
	require.ErrorIs(t, err, ErrInvalidAction, "abort needs a rollout")

	out, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: res.Rollout, Reason: "not today"})
	require.NoError(t, err)
	assert.Equal(t, "rollout aborted", out.Message)

	r := h.rollout(res.Rollout)
	assert.Equal(t, fleet.RolloutAborted, r.State)
	assert.Equal(t, map[string]fleet.HostState{"a": fleet.HostAborted, "b": fleet.HostAborted}, memberStates(r))
	assert.Equal(t, "not today", r.Reason)
	assert.NotNil(t, r.EndedAt)
	assert.Empty(t, h.assignment("a").Directives)
	assert.Empty(t, h.assignment("b").Directives)

	for _, again := range []func() error{
		func() error {
			_, err := h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: res.Rollout})

			return err
		},
		func() error {
			_, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: res.Rollout})

			return err
		},
		func() error {
			_, err := h.svc.Resume(h.ctx, "alice", ScopeRequest{Rollout: res.Rollout})

			return err
		},
	} {
		require.ErrorIs(t, again(), ErrInvalidAction, "the rollout is closed")
	}

	h.tick()
	assert.Empty(t, h.openRollouts())
}

// haltedWithTwoFailed returns a rollout that is halted with a and b failed
// and c waiting. The second failure is written as a row: a rollout halts on
// its first failure, the second comes from a lease that was open then.
func haltedWithTwoFailed(t *testing.T, restart bool) (*replay, string) {
	t.Helper()

	r := newReplay(t, restart)

	for _, name := range []string{"a", "b", "c"} {
		r.addHost(name, 0, packagesNeed("2.0"))
	}

	r.tick()

	id := r.openRollouts()[0].ID
	r.failLease("a")

	halted := r.rollout(id)
	require.Equal(t, fleet.RolloutHalted, halted.State)

	b := halted.Host("b")
	require.NotNil(t, b)

	b.State = fleet.HostFailed
	b.Error = "container did not start"

	require.NoError(t, r.store.Commit(r.ctx, &store.Changes{Rollouts: []*fleet.Rollout{halted}}))

	return r, id
}

// TestRetryOfHostsIsAboutThoseHosts pins what a retry that names hosts does,
// with and without a controller restart before every step: only the named
// members that failed return to pending; the rollout leaves its halt when no
// member is failed any more, and not before; a retry that retries nobody
// changes nothing and is refused.
func TestRetryOfHostsIsAboutThoseHosts(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run("restart="+strconv.FormatBool(restart), func(t *testing.T) {
			r, id := haltedWithTwoFailed(t, restart)
			reason := r.rollout(id).Reason
			require.NotEmpty(t, reason)

			retry := func(hosts ...string) (*ActionResult, error) {
				r.fresh()

				return r.svc.Retry(r.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: hosts})
			}

			// A host that did not fail, and one that is no member: nothing
			// is retried, nothing changes, nothing is audited.
			before, audits := r.rollout(id), len(r.store.auditActions())

			res, err := retry("c", "ghost")
			require.ErrorIs(t, err, ErrInvalidAction)
			require.ErrorContains(t, err, "no host could be retried: "+
				"c has neither failed, been cancelled nor been skipped in the rollout; "+
				"ghost has neither failed, been cancelled nor been skipped in the rollout")
			assert.Empty(t, res.Hosts)
			assert.Equal(t, before, r.rollout(id))
			assert.Len(t, r.store.auditActions(), audits)

			// One of the two failed hosts: it waits again, the rollout stays
			// halted for the other.
			res, err = retry("a", "c")
			require.NoError(t, err)
			assert.Equal(t, []string{"a"}, res.Hosts)
			assert.Equal(t, map[string]string{"c": "has neither failed, been cancelled nor been skipped in the rollout"},
				res.Rejected)
			assert.Equal(t, "1 host(s) retried; the rollout stays halted: b still failed", res.Message)

			got := r.rollout(id)
			assert.Equal(t, fleet.RolloutHalted, got.State)
			assert.Equal(t, reason, got.Reason)
			assert.Equal(t, fleet.HostPending, got.Host("a").State)
			assert.Equal(t, fleet.HostFailed, got.Host("b").State)

			entry := r.store.lastAuditOf("action.retry")
			assert.Equal(t, []string{"a"}, entry.Detail[keyHosts])
			assert.Equal(t, map[string]any{keyState: fleet.RolloutHalted}, entry.Detail[keyAfter])

			// Halted is halted: nobody is served, also not the retried host.
			r.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
			r.pass()
			r.pass()

			assert.Equal(t, fleet.RolloutHalted, r.rollout(id).State)
			assert.Empty(t, r.leased())

			// The same host again: it is not failed any more.
			_, err = retry("a")
			require.ErrorIs(t, err, ErrInvalidAction)
			assert.Equal(t, fleet.RolloutHalted, r.rollout(id).State)

			// The last failed host: the halt is lifted.
			res, err = retry("b")
			require.NoError(t, err)
			assert.Equal(t, []string{"b"}, res.Hosts)
			assert.Equal(t, "1 host(s) retried", res.Message)

			got = r.rollout(id)
			assert.Equal(t, fleet.RolloutActive, got.State)
			assert.Empty(t, got.Reason)

			entry = r.store.lastAuditOf("action.retry")
			assert.Equal(t, map[string]any{keyState: fleet.RolloutHalted, keyReason: reason},
				entry.Detail[keyBefore])
			assert.Equal(t, map[string]any{keyState: fleet.RolloutActive}, entry.Detail[keyAfter])

			r.pass()
			assert.NotEmpty(t, r.leased(), "the rollout goes on")
		})
	}
}

// TestRetryOfTheRolloutKeepsItsMeaning: without hosts every failed member
// returns to pending and the halt is lifted.
func TestRetryOfTheRolloutKeepsItsMeaning(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run("restart="+strconv.FormatBool(restart), func(t *testing.T) {
			r, id := haltedWithTwoFailed(t, restart)
			r.fresh()

			res, err := r.svc.Retry(r.ctx, "alice", RolloutHostsRequest{Rollout: id})
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"a", "b"}, res.Hosts)

			got := r.rollout(id)
			assert.Equal(t, fleet.RolloutActive, got.State)
			assert.Equal(t, fleet.HostPending, got.Host("a").State)
			assert.Equal(t, fleet.HostPending, got.Host("b").State)
		})
	}
}

func asStaff(ctx context.Context) context.Context {
	return WithAuthorization(ctx, Authorization{By: "manage", Teams: []string{"panda"}})
}

// TestHoldsPerHolder: the plain feature sets and lifts staff's hold and
// lifts any hold it names; team_manage sets and lifts the holds of the
// request's teams that own a managed container of the host, and no other.
func TestHoldsPerHolder(t *testing.T) {
	h := newHarness(t)
	host := h.addHost("a", 0, packagesNeed("2.0"))
	host.Status.Containers = []protocol.Container{
		{Name: "geth", Managed: true, Kind: protocol.KindEL, Team: "geth"},
		{Name: "beacon", Managed: true, Kind: protocol.KindCL, Team: "lighthouse"},
	}
	h.store.putHost(host)
	h.tick()

	as := func(teams ...string) context.Context {
		return WithAuthorization(h.ctx, Authorization{By: "team_manage", Teams: teams})
	}
	hold := func(ctx context.Context, reason string) {
		t.Helper()

		_, err := h.svc.Hold(ctx, "alice", HoldRequest{Hosts: []string{"a"}, Reason: reason})
		require.NoError(t, err)
		h.obeyHolds("a")
	}
	unhold := func(ctx context.Context, holders ...string) error {
		_, err := h.svc.Unhold(ctx, "bob", HoldRequest{Hosts: []string{"a"}, Holders: holders})
		h.obeyHolds("a")

		return err
	}
	holders := func() []string { return h.store.host("a").Status.Hold.Holders() }

	// One hold per team of the request that owns a container of the host.
	hold(as("geth", "lighthouse", "other"), "ours")
	assert.Equal(t, []string{"geth", "lighthouse"}, holders())

	entry := h.store.lastAuditOf("action.hold")
	assert.Equal(t, "lighthouse", entry.Detail[keyHolder])
	assert.Equal(t, map[string]any{"held": false}, entry.Detail[keyBefore])
	assert.Equal(t, map[string]any{"held": true, "by": "alice", keyReason: "ours"}, entry.Detail[keyAfter])
	assert.Equal(t, "team_manage", entry.Detail[keyAuthorizedBy])
	assert.NotEmpty(t, entry.Detail["directive"])

	hold(asStaff(h.ctx), "staff")
	assert.Equal(t, []string{fleet.HolderStaff, "geth", "lighthouse"}, holders())
	assert.Equal(t, "held by staff, geth, lighthouse", h.store.host("a").HeldReason())

	// A team lifts its own holds, all of them or the ones it names; never
	// staff's or another team's. A refusal changes and records nothing.
	audits := len(h.store.auditActions())

	err := unhold(as("geth"), fleet.HolderStaff)
	require.ErrorIs(t, err, ErrForbidden)
	require.ErrorContains(t, err, "team_manage does not lift the hold of staff on host a")
	require.ErrorContains(t, unhold(as("geth"), "lighthouse"), "does not lift the hold of lighthouse on host a")
	require.ErrorContains(t, unhold(as("other")), "team_manage does not cover host a")

	_, err = h.svc.Hold(as("other"), "alice", HoldRequest{Hosts: []string{"a"}})
	require.ErrorIs(t, err, ErrForbidden)
	assert.Len(t, h.store.auditActions(), audits)

	require.NoError(t, unhold(as("geth", "lighthouse"), "geth"))
	assert.Equal(t, []string{fleet.HolderStaff, "lighthouse"}, holders())

	entry = h.store.lastAuditOf("action.unhold")
	assert.Equal(t, "geth", entry.Detail[keyHolder])
	assert.Equal(t, map[string]any{"held": true, "by": "alice", keyReason: "ours"}, entry.Detail[keyBefore])
	assert.Equal(t, map[string]any{"held": false, "by": "bob", keyReason: ""}, entry.Detail[keyAfter])

	// The plain feature lifts staff's hold by default, a team's by name.
	require.NoError(t, unhold(asStaff(h.ctx)))
	assert.Equal(t, []string{"lighthouse"}, holders())

	require.NoError(t, unhold(asStaff(h.ctx), "lighthouse"))
	assert.Empty(t, holders())

	// A request allowed by the plain feature of a login that also is in
	// teams sets staff's hold: the feature decides, not the teams.
	hold(WithAuthorization(h.ctx, Authorization{By: "manage", Teams: []string{"lighthouse"}}), "x")
	assert.Equal(t, []string{fleet.HolderStaff}, holders())

	// A hold names no holders.
	_, err = h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a"}, Holders: []string{"geth"}})
	require.ErrorIs(t, err, ErrInvalidAction)

	// The holds are the agent's: a restart of the controller keeps them.
	hold(as("geth"), "ours")

	h.restart()
	h.tick()
	assert.Equal(t, []string{fleet.HolderStaff, "geth"}, holders())
	assert.Equal(t, "ours", h.store.host("a").HoldOf("geth").Reason)
}

// TestUnholdOfAHostThatIsNotHeld changes nothing and writes no audit entry.
func TestUnholdOfAHostThatIsNotHeld(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0)
	h.addHost("b", 0)

	_, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"b"}})
	require.NoError(t, err)

	audits := h.store.auditActions()

	res, err := h.svc.Unhold(h.ctx, "alice", HoldRequest{Hosts: []string{"a"}})
	require.NoError(t, err)
	assert.Empty(t, res.Hosts)
	assert.Equal(t, "release requested on 0 host(s), waiting for the hosts; 1 host(s) had no such hold", res.Message)
	assert.Equal(t, audits, h.store.auditActions())

	// The hold of "b" is only requested: lifting it takes the request back.
	res, err = h.svc.Unhold(h.ctx, "alice", HoldRequest{Hosts: []string{"a", "b"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, res.Hosts)
	assert.Equal(t, "release requested on 1 host(s), waiting for the hosts; 1 host(s) had no such hold", res.Message)
	assert.Equal(t, append(audits, "action.unhold"), h.store.auditActions())
}

// TestRetryOfASkippedHostLiftsNoHalt: a rollout halts on b's failure and b is
// skipped, so no member is failed and the rollout stays halted. Skipping a
// waiting host and retrying it does not lift that halt: no failure was
// retried. A retry of the rollout does.
func TestRetryOfASkippedHostLiftsNoHalt(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run("restart="+strconv.FormatBool(restart), func(t *testing.T) {
			r := newReplay(t, restart)

			for _, name := range []string{"a", "b", "c"} {
				r.addHost(name, 0, packagesNeed("2.0"))
			}

			r.tick()

			id := r.openRollouts()[0].ID
			failed := r.leased()
			require.Len(t, failed, 1)
			r.failLease(failed[0])
			require.Equal(t, fleet.RolloutHalted, r.rollout(id).State)

			waiting := "a"
			if failed[0] == "a" {
				waiting = "c"
			}

			// The failed host is skipped by its team: still halted.
			r.skip(id, failed[0])
			r.pass()
			require.Equal(t, fleet.RolloutHalted, r.rollout(id).State)
			require.Empty(t, failedMembers(r.rollout(id)))

			// Another team skips its waiting host and retries it.
			r.skip(id, waiting)
			r.fresh()

			res, err := r.svc.Retry(r.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{waiting}})
			require.NoError(t, err)
			assert.Equal(t, []string{waiting}, res.Hosts)
			assert.Equal(t, "1 host(s) retried; the rollout stays halted: none of the retried hosts had failed "+
				"(retry the rollout to lift its halt)", res.Message)

			got := r.rollout(id)
			assert.Equal(t, fleet.RolloutHalted, got.State)
			assert.Equal(t, fleet.HostPending, got.Host(waiting).State)

			r.pass()
			assert.Equal(t, fleet.RolloutHalted, r.rollout(id).State)
			assert.Empty(t, r.leased())

			// The skip of the failed host, taken back: no failure either.
			r.fresh()

			_, err = r.svc.Retry(r.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{failed[0]}})
			require.NoError(t, err)
			assert.Equal(t, fleet.RolloutHalted, r.rollout(id).State)

			// The rollout as a whole: lifted.
			r.fresh()

			_, err = r.svc.Retry(r.ctx, "alice", RolloutHostsRequest{Rollout: id})
			require.NoError(t, err)
			assert.Equal(t, fleet.RolloutActive, r.rollout(id).State)
		})
	}
}

// TestActionsRecordTheirAuthorization: every action writes what allowed it
// and the teams of the login into its audit entry, also the actions that
// commit their entry together with their change.
func TestActionsRecordTheirAuthorization(t *testing.T) {
	h := newHarness(t)

	for _, name := range []string{"a", "b", "c"} {
		h.addHost(name, 0, packagesNeed("2.0"))
		h.sync(name, func(st *protocol.SyncRequest) {
			st.Containers = []protocol.Container{{Name: "geth", Managed: true, State: "running"}}
		})
	}

	h.tick()

	id := h.openRollouts()[0].ID
	teams := []string{"geth", "lighthouse"}

	as := func(by string) context.Context {
		return WithAuthorization(h.ctx, Authorization{By: by, Teams: teams})
	}

	recorded := func(action, by string) {
		t.Helper()

		entry := h.store.lastAuditOf(action)
		require.Equal(t, action, entry.Action)
		assert.Equal(t, by, entry.Detail[keyAuthorizedBy], action)
		assert.Equal(t, teams, entry.Detail[keyTeams], action)
		assert.Equal(t, "alice", entry.Actor)
	}

	_, err := h.svc.Pause(as("manage"), "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	recorded("action.pause", "manage")

	_, err = h.svc.Resume(as("manage"), "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	recorded("action.resume", "manage")

	h.failLease("a")

	_, err = h.svc.Retry(as("team_manage"), "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"a"}})
	require.NoError(t, err)
	recorded("action.retry", "team_manage")

	_, err = h.svc.Skip(as("team_manage"), "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"c"}})
	require.NoError(t, err)
	recorded("action.skip", "team_manage")

	h.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
	h.tick()

	lease := openLeaseOf(t, h, "")
	_, err = h.svc.EndLease(as("manage"), "alice", EndLeaseRequest{Lease: lease})
	require.NoError(t, err)
	recorded("action.end-lease", "manage")

	_, err = h.svc.Abort(as("manage"), "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	recorded("action.abort", "manage")

	_, err = h.svc.Roll(as("team_manage"), "alice", RollRequest{Hosts: []string{"b"}, Staggered: true})
	require.NoError(t, err)
	recorded("action.roll", "team_manage")

	_, err = h.svc.Reboot(as("manage"), "alice", RebootRequest{Hosts: []string{"b"}, Force: true})
	require.NoError(t, err)
	recorded("action.reboot", "manage")

	_, err = h.svc.Pause(as("admin"), "alice", ScopeRequest{})
	require.NoError(t, err)
	recorded("action.pause", "admin")

	settings, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)

	settings.MinPeers = 7
	require.NoError(t, replaceSettings(as("admin"), h.svc, "alice", settings))
	recorded("settings.update", "admin")

	// What the scheduler writes by itself carries none.
	h.tick()

	for _, e := range auditBy(h, actorScheduler) {
		assert.NotContains(t, e.Detail, keyAuthorizedBy, e.Action)
	}
}

// openLeaseOf returns the ID of an open lease, of the host when one is named.
func openLeaseOf(t *testing.T, h *harness, host string) string {
	t.Helper()

	leases, err := h.store.ListLeases(h.ctx, store.LeaseFilter{OpenOnly: true, Host: host})
	require.NoError(t, err)
	require.NotEmpty(t, leases, "no open lease")

	return leases[0].ID
}

// auditBy returns the audit entries of one actor.
func auditBy(h *harness, actor string) []fleet.AuditEntry {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	out := make([]fleet.AuditEntry, 0, len(h.store.audit))

	for _, e := range h.store.audit {
		if e.Actor == actor {
			out = append(out, e)
		}
	}

	return out
}

func TestRollAction(t *testing.T) {
	h := newHarness(t)

	lighthouse := container("lighthouse", imgLighthouse, digestA, digestA, h.now)
	geth := container("geth", imgGeth, digestA, digestA, h.now)
	custom := container("debug", "someone/debug:x", digestA, "", h.now)
	custom.Managed = false
	unmanaged := protocol.Container{Name: "node-exporter", Image: "prom/node-exporter"}

	withContainers := func(cs ...protocol.Container) func(st *protocol.SyncRequest) {
		return func(st *protocol.SyncRequest) { st.Containers = cs }
	}

	h.addHost("ok", 10)
	h.sync("ok", withContainers(lighthouse, geth, custom, unmanaged))
	h.addHost("held", 0)
	h.sync("held", func(st *protocol.SyncRequest) {
		st.Containers = []protocol.Container{geth}
		st.Hold = protocol.Hold{Local: true, Holds: []protocol.HostHold{{Holder: protocol.HolderHost}}}
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
	// The scheduler has been up for as long: the silence is the host's.
	h.svc.since = silent.LastSeen

	res, err := h.svc.Roll(h.ctx, "alice", RollRequest{All: true, Reason: "new config"})
	require.NoError(t, err)

	assert.Equal(t, []string{"ok"}, res.Hosts)
	assert.Equal(t, map[string]string{
		"held":       "held by the host",
		"noverb":     "verb roll not allowed by the agent",
		"onlycustom": "no managed container to roll",
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

	// One roll directive per managed container.
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
		require.Len(t, all, 1, "the unmanaged container is not rolled")
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
		assert.Equal(t, map[string]string{"held": "held by the host"}, res.Rejected)

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
	boot.Roles = []string{"bootnode"}
	h.store.putHost(boot)
	h.advance(0)

	current, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)

	current.MaxUnavailablePercent = 100
	current.MaxLeasedPercent = 100
	require.NoError(t, replaceSettings(h.ctx, h.svc, "alice", current))

	res, err := h.svc.Reboot(h.ctx, "alice", RebootRequest{All: true, Staggered: true, Reason: "kernel"})
	require.NoError(t, err)
	assert.Equal(t, []string{"boot", "tool", "v1", "v2"}, res.Hosts)
	assert.Empty(t, res.Rejected)
	assert.Equal(t, "action.reboot", h.store.lastAudit().Action)

	r := h.rollout(res.Rollout)
	assert.False(t, r.Force)

	for i := range r.Hosts {
		assert.Equal(t, fleet.StageRest, r.Hosts[i].Stage, "the canary is chosen by the scheduler")
	}

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

	// The staggered rollout runs its two stages: the canary alone, then the
	// rest at once, the bootnode among them.
	leased := func() []string {
		out := make([]string, 0, len(res.Hosts))

		for _, name := range res.Hosts {
			if h.assignment(name).Lease != nil {
				out = append(out, name)
			}
		}

		return out
	}

	h.tick()
	require.Equal(t, []string{"v1"}, leased())
	assert.Equal(t, "v1", h.rollout(res.Rollout).Canary)
	assert.Equal(t, fleet.StageCanary, h.rollout(res.Rollout).Host("v1").Stage)

	h.finish("v1")
	require.Equal(t, []string{"boot", "tool", "v2"}, leased())

	for _, name := range leased() {
		h.finish(name)
	}

	assert.Equal(t, fleet.RolloutCompleted, h.rollout(res.Rollout).State)
	assert.Empty(t, h.openRollouts(), "manual needs open no observed-cause rollout")
}

func TestHoldAndUnholdActions(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0)

	_, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a", "nope"}})
	require.ErrorIs(t, err, ErrUnknownHost)
	assert.Empty(t, h.assignment("a").Directives, "nothing applied")

	_, err = h.svc.Hold(h.ctx, "alice", HoldRequest{})
	require.ErrorIs(t, err, ErrInvalidAction)

	res, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a", "b"}, Reason: "debugging"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, res.Hosts)
	assert.Equal(t, "hold requested on 2 host(s), waiting for the hosts", res.Message)
	assert.Equal(t, []string{"action.hold", "action.hold"}, h.store.auditActions())
	assert.False(t, h.store.host("a").Held(), "the agent owns its holds")

	entry := h.store.lastAudit()
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, "b", entry.Target)
	assert.Equal(t, fleet.HolderStaff, entry.Detail[keyHolder])
	assert.Equal(t, map[string]any{"held": false}, entry.Detail["before"])
	assert.Equal(t, map[string]any{"held": true, "by": "alice", "reason": "debugging"}, entry.Detail["after"])

	// A host asked for a hold is never granted, already before its agent
	// answers; the plan shows the request until it does.
	h.tick()
	a := h.assignment("a")
	assert.Nil(t, a.Lease)
	assert.Equal(t, "hold requested, waiting for the host", a.Queue.Reason)
	require.Len(t, a.Directives, 1)
	assert.Equal(t, protocol.Directive{
		ID: a.Directives[0].ID, Kind: protocol.DirectiveHold, Holder: fleet.HolderStaff, Reason: "debugging",
		By: &protocol.Actor{User: "alice"},
	}, a.Directives[0])
	assert.Equal(t, []PendingHold{{Holder: fleet.HolderStaff, By: "alice", Reason: "debugging", At: h.now}},
		h.svc.Plan().Hosts["b"].HoldRequests)
	assert.Equal(t, SlotHeld, h.svc.Plan().Hosts["b"].Slot)

	h.obeyHolds("a")
	h.obeyHolds("b")
	assert.Equal(t, "held by staff", h.assignment("a").Queue.Reason)
	assert.Empty(t, h.svc.Plan().Hosts["b"].HoldRequests, "answered")
	assert.Equal(t, fleet.HolderStaff, h.store.lastAuditOf("directive.accepted").Detail["holder"])

	res, err = h.svc.Unhold(h.ctx, "bob", HoldRequest{Hosts: []string{"a"}})
	require.NoError(t, err)
	assert.Equal(t, "release requested on 1 host(s), waiting for the hosts", res.Message)
	assert.Equal(t, "action.unhold", h.store.lastAudit().Action)

	// Until the agent answers it is held as it says.
	h.tick()
	assert.Nil(t, h.assignment("a").Lease)
	assert.Equal(t, []PendingHold{{Holder: fleet.HolderStaff, Release: true, By: "bob", At: h.now}},
		h.svc.Plan().Hosts["a"].HoldRequests)

	h.obeyHolds("a")
	assert.False(t, h.store.host("a").Held())
	assert.NotNil(t, h.assignment("a").Lease)
}

func TestPauseAndResumeActions(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, rebootNeed())

	current, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)

	current.PolicyReboot = fleet.PolicyManual
	require.NoError(t, replaceSettings(h.ctx, h.svc, "alice", current))

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

		// A rollout that is closed is neither paused nor resumed.
		res, err := h.svc.Reboot(h.ctx, "alice", RebootRequest{All: true, Staggered: true})
		require.NoError(t, err)

		_, err = h.svc.Abort(h.ctx, "alice", ScopeRequest{Rollout: res.Rollout})
		require.NoError(t, err)

		_, err = h.svc.Pause(h.ctx, "alice", ScopeRequest{Rollout: res.Rollout})
		require.ErrorIs(t, err, ErrInvalidAction)

		_, err = h.svc.Resume(h.ctx, "alice", ScopeRequest{Rollout: res.Rollout})
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
	assert.Equal(t, "broken disk", m.Error)
	assert.Equal(t, "alice", m.SkippedBy)
	assert.Equal(t, "action.skip", h.store.lastAudit().Action)

	// The skipped host keeps reporting the need: it is never granted, and it
	// keeps the rollout open (TestSkipKeepsTheRolloutOpenWhileTheHostHasTheNeed).
	h.finish("leased")
	assert.Nil(t, h.assignment("waiting").Lease)
	assert.Equal(t, fleet.RolloutActive, h.rollout(id).State)

	_, err = h.svc.Skip(h.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"waiting"}})
	require.ErrorIs(t, err, ErrInvalidAction, "already skipped")

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
	assert.Equal(t, map[string]string{"b": "has neither failed, been cancelled nor been skipped in the rollout"}, res.Rejected)

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

		// The skipped host still reports the need and is not served again.
		h2.advance(time.Hour)
		h2.tick()
		assert.Equal(t, fleet.RolloutActive, h2.rollout(id).State)
		assert.Equal(t, fleet.HostSkipped, h2.rollout(id).Host("a").State)
		assert.Nil(t, h2.assignment("a").Lease)

		_, err = h2.svc.Retry(h2.ctx, "alice", RolloutHostsRequest{Rollout: id})
		require.ErrorIs(t, err, ErrInvalidAction, "nothing left to retry")
	})

	t.Run("unknown rollout", func(t *testing.T) {
		_, err := h.svc.Retry(h.ctx, "alice", RolloutHostsRequest{Rollout: "nope"})
		require.ErrorIs(t, err, ErrUnknownRollout)
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
	// The hold of "a" passes, the hold of "b" fails.
	h.store.failSkip, h.store.failNext = 1, 1

	res, err := h.svc.Hold(h.ctx, "alice", HoldRequest{Hosts: []string{"a", "b", "c"}, Reason: "debugging"})
	require.ErrorIs(t, err, errWrite)
	require.NotNil(t, res, "the caller learns what was changed")
	assert.Equal(t, []string{"a"}, res.Hosts)
	assert.Contains(t, res.Message, "stopped at b")
	h.tick()
	assert.Len(t, h.assignment("a").Directives, 1)
	assert.Empty(t, h.assignment("b").Directives)
	assert.Empty(t, h.assignment("c").Directives)
	assert.Equal(t, []string{"action.hold"}, h.store.auditActions())
}

// The image policy of a client governs the image rollouts of its containers;
// every other rollout keeps answering to policyImage.
func TestClientPolicyAction(t *testing.T) {
	h := newHarness(t)

	for _, tt := range []struct{ host, image, client string }{
		{"a", "ethpandaops/geth:master", "geth"},
		{"b", "sigp/lighthouse:latest", "lighthouse"},
	} {
		h.addHost(tt.host, 0)
		h.sync(tt.host, func(st *protocol.SyncRequest) {
			c := container("c", tt.image, digestOld, digestA, h.now)
			c.Team = tt.client
			st.Containers = []protocol.Container{c}
			n := imageNeed("c", tt.image, digestA)
			n.Image.Team = tt.client
			st.Needs = []protocol.Need{n}
		})
	}

	res, err := h.svc.SetClientPolicy(h.ctx, "alice", ClientPolicyRequest{
		Client: "geth", Policy: fleet.PolicyManual, Reason: "release week",
	})
	require.NoError(t, err)
	assert.Equal(t, "image policy of geth: manual", res.Message)

	entry := h.store.lastAudit()
	assert.Equal(t, "action.client-policy", entry.Action)
	assert.Equal(t, "release week", entry.Detail["reason"])

	audits := len(h.store.auditActions())
	_, err = h.svc.SetClientPolicy(h.ctx, "alice", ClientPolicyRequest{
		Client: "geth", Policy: fleet.PolicyManual,
	})
	require.NoError(t, err)
	assert.Len(t, h.store.auditActions(), audits, "setting what is set changes and audits nothing")

	h.tick()

	geth := h.openRollout(imageKey("geth", "ethpandaops/geth:master", digestA))
	assert.Equal(t, "geth", geth.Team)
	assert.Equal(t, "waiting for approval (policy manual)", h.assignment("a").Queue.Reason)
	assert.NotNil(t, h.assignment("b").Lease, "lighthouse follows policyImage, which is auto")

	res, err = h.svc.SetClientPolicy(h.ctx, "alice", ClientPolicyRequest{Client: "geth"})
	require.NoError(t, err)
	assert.Equal(t, "image policy of geth: the default of each kind", res.Message)

	got, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.Empty(t, got.PolicyClients)

	_, err = h.svc.SetClientPolicy(h.ctx, "alice", ClientPolicyRequest{Client: "Geth", Policy: "auto"})
	require.ErrorIs(t, err, ErrInvalidAction)
}

// An override action queues one directive for the host, with who asked. It
// opens no rollout: the host's own need does, once its agent wrote the file.
// TestClientOverride: one directive per host for its container of the client
// and kind, in one commit; the hosts that cannot take it are listed.
func TestClientOverride(t *testing.T) {
	h := newHarness(t)

	for _, name := range []string{"a", "b", "c", "d"} {
		h.addHost(name, 0)
	}

	beacon := func(name string, overridable bool) protocol.Container {
		return protocol.Container{
			Name: name, Managed: true, Kind: protocol.KindCL, Team: "teku", Overridable: overridable,
		}
	}

	h.sync("a", func(st *protocol.SyncRequest) { st.Containers = []protocol.Container{beacon("teku", true)} })
	h.sync("b", func(st *protocol.SyncRequest) { st.Containers = []protocol.Container{beacon("beacon", true)} })
	h.sync("c", func(st *protocol.SyncRequest) { st.Containers = []protocol.Container{beacon("teku", false)} })
	h.sync("d", func(st *protocol.SyncRequest) {
		st.Containers = []protocol.Container{{Name: "geth", Managed: true, Kind: protocol.KindEL, Team: "geth"}}
	})

	res, err := h.svc.ClientOverride(h.ctx, "sam@example.org", ClientOverrideRequest{
		Client: "teku", Kind: protocol.KindCL, Hosts: []string{"a", "b", "c", "d", "ghost"},
		Part: protocol.OverrideFlags, AddFlags: []string{"--log=debug"}, Reason: "debug",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, res.Hosts)
	assert.Equal(t, map[string]string{
		"c":     "container teku on host c cannot be overridden: overrides are off on the host",
		"d":     "host d runs no cl container of teku",
		"ghost": "unknown host",
	}, res.Rejected)
	assert.Equal(t, "sent to 2 of 5 hosts: each agent decides, and its container follows under the host's next lease",
		res.Message)

	for host, container := range map[string]string{"a": "teku", "b": "beacon"} {
		directives := h.assignment(host).Directives
		require.Len(t, directives, 1, host)
		assert.Equal(t, container, directives[0].Container)
		assert.Equal(t, []string{"--log=debug"}, directives[0].Override.AddFlags)
		assert.Equal(t, h.now.Add(DefaultOverrideDuration).UTC().Truncate(time.Second), *directives[0].Override.Until)
	}

	assert.Empty(t, h.assignment("c").Directives)
	assert.Equal(t, "action.flag-override", h.store.lastAudit().Action)

	// Clearing needs no allowance and has no end.
	res, err = h.svc.ClientOverride(h.ctx, "sam@example.org", ClientOverrideRequest{
		Client: "teku", Kind: protocol.KindCL, Hosts: []string{"c"}, Part: protocol.OverrideFlags,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, res.Hosts)
	assert.Nil(t, h.assignment("c").Directives[0].Override.Until)

	// An end beyond the maximum refuses the whole request.
	far := h.now.Add(8 * 24 * time.Hour)
	_, err = h.svc.ClientOverride(h.ctx, "sam@example.org", ClientOverrideRequest{
		Client: "teku", Kind: protocol.KindCL, Hosts: []string{"a"}, Part: protocol.OverrideImage,
		Image: "consensys/teku:fix", Until: &far,
	})
	require.ErrorIs(t, err, ErrInvalidAction)
}

func TestOverrideActions(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0)
	h.sync("a", func(st *protocol.SyncRequest) {
		st.Containers = []protocol.Container{
			{Name: "geth", Managed: true, Kind: protocol.KindEL, Team: "geth", Overridable: true},
			{Name: "beacon", Managed: true, Kind: protocol.KindCL, Team: "teku", Overridable: true},
			{Name: "plain", Managed: true, Kind: "snooper-el"},
			{Name: "debug"},
		}
	})

	ctx := WithAuthorization(h.ctx, Authorization{By: "team_manage_image", Teams: []string{"geth"}})

	res, err := h.svc.ImageOverride(ctx, "sam@example.org", ImageOverrideRequest{
		Host: "a", Container: "geth", Image: "ethpandaops/geth:fix", Reason: "sync",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, res.Hosts)
	assert.Empty(t, res.Rollout)

	directives := h.assignment("a").Directives
	require.Len(t, directives, 1)

	d := directives[0]
	assert.Equal(t, protocol.DirectiveOverride, d.Kind)
	assert.Equal(t, "geth", d.Container)
	assert.Equal(t, &protocol.Actor{User: "sam@example.org", Teams: []string{"geth"}, Feature: "team_manage_image"}, d.By)
	assert.Equal(t, protocol.OverrideImage, d.Override.Part)
	assert.Equal(t, h.now.Add(DefaultOverrideDuration).UTC().Truncate(time.Second), *d.Override.Until,
		"an override always has an end")

	entry := h.store.lastAudit()
	assert.Equal(t, "action.image-override", entry.Action)
	assert.Equal(t, "a", entry.Target)
	assert.Equal(t, "team_manage_image", entry.Detail["authorizedBy"])

	h.tick()
	assert.Empty(t, h.openRollouts())

	far, past := h.now.Add(8*24*time.Hour), h.now.Add(-time.Minute)

	for name, tt := range map[string]struct {
		call func() error
		want string
	}{
		"a container without a team": {func() error {
			_, callErr := h.svc.ImageOverride(ctx, "sam", ImageOverrideRequest{Host: "a", Container: "plain", Image: "x/y:z"})

			return callErr
		}, "container plain on host a cannot be overridden: it has no team"},
		"flags of a container without a team": {func() error {
			_, callErr := h.svc.FlagOverride(ctx, "sam", FlagOverrideRequest{Host: "a", Container: "plain", AddFlags: []string{"--x"}})

			return callErr
		}, "container plain on host a cannot be overridden: it has no team"},
		"not a managed container": {func() error {
			_, callErr := h.svc.ImageOverride(ctx, "sam", ImageOverrideRequest{Host: "a", Container: "debug", Image: "x/y:z"})

			return callErr
		}, "manages no container debug"},
		"an end too far away": {func() error {
			_, callErr := h.svc.ImageOverride(ctx, "sam", ImageOverrideRequest{Host: "a", Container: "geth", Image: "x/y:z", Until: &far})

			return callErr
		}, "at most"},
		"an end that has passed": {func() error {
			_, callErr := h.svc.FlagOverride(ctx, "sam", FlagOverrideRequest{Host: "a", Container: "beacon", AddFlags: []string{"--x"}, Until: &past})

			return callErr
		}, "in the future"},
	} {
		t.Run(name, func(t *testing.T) {
			callErr := tt.call()
			require.ErrorIs(t, callErr, ErrInvalidAction)
			assert.ErrorContains(t, callErr, tt.want)
		})
	}

	_, err = h.svc.ImageOverride(ctx, "sam", ImageOverrideRequest{Host: "nope", Container: "geth", Image: "x/y:z"})
	require.ErrorIs(t, err, ErrUnknownHost)

	// Taking an override away needs no opt-in of the host and no end.
	_, err = h.svc.FlagOverride(ctx, "sam", FlagOverrideRequest{Host: "a", Container: "plain", Until: &far})
	require.NoError(t, err)

	directives = h.assignment("a").Directives
	require.Len(t, directives, 2)
	assert.Nil(t, directives[1].Override.Until)

	// The agent's answers are audited; a directive nobody answers is
	// withdrawn.
	h.sync("a", func(st *protocol.SyncRequest) {
		st.DirectiveAcks = []protocol.DirectiveAck{
			{ID: d.ID, Status: protocol.DirectiveRejected, Error: "image not in an allowed repository"},
		}
	})
	h.tick()

	rejected := h.store.lastAuditOf("directive.rejected")
	assert.Equal(t, "a", rejected.Target)
	assert.Equal(t, "image not in an allowed repository", rejected.Detail["error"])
	require.Len(t, h.assignment("a").Directives, 1)

	h.advance(overrideAnswerTimeout + time.Minute)
	h.tick()

	assert.Empty(t, h.assignment("a").Directives)
	assert.Equal(t, "the agent did not answer in time", h.store.lastAuditOf("directive.cancelled").Detail["error"])
}

func TestMarkOffline(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.tick()

	until := h.now.Add(time.Hour)

	_, err := h.svc.MarkOffline(asStaff(h.ctx), "alice",
		OfflineRequest{Hosts: []string{"a"}, Reason: "debugging", Until: &until})
	require.NoError(t, err)

	mark := h.store.host("a").Offline
	require.NotNil(t, mark)
	assert.Equal(t, "debugging", mark.Reason)
	assert.False(t, h.store.host("a").Held(), "the mark is no hold")

	entry := h.store.lastAuditOf("action.mark-offline")
	assert.Equal(t, map[string]any{"offline": false}, entry.Detail[keyBefore])
	after, ok := entry.Detail[keyAfter].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, after["offline"])

	_, err = h.svc.UnmarkOffline(asStaff(h.ctx), "bob", OfflineRequest{Hosts: []string{"a"}})
	require.NoError(t, err)
	assert.Nil(t, h.store.host("a").Offline)
	assert.NotNil(t, h.store.lastAuditOf("action.unmark-offline"))
}

// A manual roll is a team's when every container it rolls is of that team,
// and of their kind when they share one; a roll across teams is nobody's.
func TestManualRollRecordsItsTeam(t *testing.T) {
	h := newHarness(t)

	el := container("geth", imgGeth, digestA, digestA, h.now)
	el.Kind, el.Team = protocol.KindEL, "geth"
	vc := container("geth-vc", imgGeth, digestA, digestA, h.now)
	vc.Kind, vc.Team = protocol.KindVC, "geth"
	cl := container("lighthouse", imgLighthouse, digestA, digestA, h.now)
	cl.Kind, cl.Team = protocol.KindCL, "lighthouse"

	h.addHost("a", 0)
	h.sync("a", func(st *protocol.SyncRequest) { st.Containers = []protocol.Container{el, vc, cl} })
	h.addHost("b", 0)
	h.sync("b", func(st *protocol.SyncRequest) { st.Containers = []protocol.Container{el} })

	roll := func(req RollRequest) *fleet.Rollout {
		t.Helper()

		res, err := h.svc.Roll(h.ctx, "alice", req)
		require.NoError(t, err)

		return h.rollout(res.Rollout)
	}

	r := roll(RollRequest{Hosts: []string{"a", "b"}, Containers: []string{"geth"}, Staggered: true})
	assert.Equal(t, "geth", r.Team)
	assert.Equal(t, protocol.KindEL, r.ContainerKind)

	r = roll(RollRequest{Hosts: []string{"a"}, Containers: []string{"geth", "geth-vc"}, Staggered: true})
	assert.Equal(t, "geth", r.Team)
	assert.Empty(t, r.ContainerKind, "two kinds")

	r = roll(RollRequest{Hosts: []string{"a"}, Staggered: true})
	assert.Empty(t, r.Team, "containers of two teams")
	assert.Empty(t, r.ContainerKind)
}
