package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The tests in this file pin the findings of the seventh review: one rule
// for a lease that a person ended, whenever they clicked; a hold that cannot
// be lifted from under a cancel; and the corners of taking a lease up again.

// M1 of the seventh review: what an ended lease says does not depend on when
// the person clicked. The agent reports its operation done; in one run a
// reconcile sees the report before the click (the lease is verifying when it
// is ended), in the other the click comes first (it is still running), in the
// third the click comes during the last step. The lease, the rollout, the
// member and what is broken are the same in all three.
func TestReview7EndedLeaseIsJudgedByTheReportNotByTheClick(t *testing.T) {
	type outcome struct {
		lease          fleet.LeaseState
		reason         string
		steps          []protocol.Step
		rollout        fleet.RolloutState
		rolloutReason  string
		member         fleet.HostState
		memberLease    bool
		memberError    string
		brokenPackages bool
		held           bool
		leased         []string
	}

	play := func(t *testing.T, restart bool, order string) (*replay, outcome) {
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
		require.Equal(t, fleet.LeaseRunning, r.storedLease(lease).State)

		end := func(want fleet.LeaseState) {
			_, err := r.endLeaseID("alice", lease)
			require.NoError(t, err)
			require.Equal(t, want, r.storedLease(lease).Cancelled.State)
		}
		report := func() { r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone) }

		switch order {
		case "report, reconcile, click":
			report()
			r.pass()
			require.Equal(t, fleet.LeaseVerifying, r.storedLease(lease).State)
			end(fleet.LeaseVerifying)
		case "report, click, reconcile":
			report()
			end(fleet.LeaseRunning)
		case "click, report, reconcile":
			end(fleet.LeaseRunning)
			r.pass()
			report()
		}

		r.agentHeld("a")
		r.pass()

		l := r.storedLease(lease)
		m := r.rollout(rollout).Host("a")

		return r, outcome{
			lease: l.State, reason: l.Error, steps: l.Operation.Steps,
			rollout: r.rollout(rollout).State, rolloutReason: r.rollout(rollout).Reason,
			member: m.State, memberLease: m.LeaseID == lease, memberError: m.Error,
			brokenPackages: newLedger([]*fleet.Lease{l}).brokenSince("a", groupPackages) != nil,
			held:           r.store.host("a").HoldWanted,
			leased:         r.leased(),
		}
	}

	want := outcome{
		lease: fleet.LeaseFailed, reason: "ended by operator",
		steps:   []protocol.Step{{NeedID: packagesNeed("2.0").ID, Status: protocol.StepDone}},
		rollout: fleet.RolloutHalted, rolloutReason: "a: ended by operator",
		member: fleet.HostFailed, memberLease: true, memberError: "ended by operator",
		brokenPackages: true, held: true, leased: []string{},
	}

	for _, order := range []string{
		"report, reconcile, click", "report, click, reconcile", "click, report, reconcile",
	} {
		t.Run(order, func(t *testing.T) {
			_, got := play(t, false, order)
			assert.Equal(t, want, got)

			sameAfterRestarts(t, func(t *testing.T, restart bool) *replay {
				t.Helper()

				r, got := play(t, restart, order)
				assert.Equal(t, want, got)

				return r
			})
		})
	}
}

// M2: the hold is part of the cancel. It cannot be lifted while the lease is
// open, for none of the hosts of the action; once the lease has ended a
// person lifts it.
func TestReview7UnholdDoesNotUndoACancel(t *testing.T) {
	play := func(t *testing.T, restart bool) *replay {
		t.Helper()

		r, lease := mixedFleet(t, restart)

		r.hold("b")

		_, err := r.endLeaseID("alice", lease)
		require.NoError(t, err)

		entries := len(r.store.auditActions())

		for _, hosts := range [][]string{{"a"}, {"b", "a"}} {
			r.fresh()

			res, err := r.svc.Unhold(r.ctx, "carol", HoldRequest{Hosts: hosts})
			require.ErrorIs(t, err, ErrInvalidAction)
			assert.Nil(t, res)
			assert.Contains(t, err.Error(), "lease "+lease+" of a was ended by alice and is still open (running)")
			r.dump()
		}

		assert.True(t, r.store.host("a").HoldWanted)
		assert.True(t, r.store.host("b").HoldWanted, "a refused action changes no host")
		assert.Len(t, r.store.auditActions(), entries, "nor is it audited as done")

		// The agent stops; the lease ends. Now the hold can be lifted.
		r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepCancelled, protocol.StepCancelled)
		r.agentHeld("a")
		r.pass()
		require.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)
		assert.True(t, r.store.host("a").HoldWanted, "the end of the lease does not lift the hold")

		r.unhold("a", packagesNeed("2.0"))
		assert.False(t, r.store.host("a").HoldWanted)

		return r
	}

	play(t, false)
	sameAfterRestarts(t, play)
}

// M2: carol held the host, alice ended its lease, carol lifts her hold while
// the agent still works. Before, that left the host unheld when the lease
// ended, and it was granted the same work again at once. Now carol's unhold
// is refused while the lease is open, and the reason of the hold she finds
// afterwards names the cancel.
func TestReview7AnEarlierHoldDoesNotLetTheHostBeGrantedAgain(t *testing.T) {
	play := func(t *testing.T, restart bool) *replay {
		t.Helper()

		r, lease := mixedFleet(t, restart)

		r.fresh()

		_, err := r.svc.Hold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}, Reason: "disk is dying"})
		require.NoError(t, err)
		r.dump()

		_, err = r.endLeaseID("alice", lease)
		require.NoError(t, err)

		r.fresh()

		_, err = r.svc.Unhold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}})
		require.ErrorIs(t, err, ErrInvalidAction)
		r.dump()

		// The agent stops without having begun the packages; its needs are
		// what they were. Nothing is granted to it again.
		r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone, protocol.StepCancelled)
		r.pass()
		require.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)

		for _, id := range []string{
			r.openRollout(imageKey(imgLighthouse, digestA)).ID, r.openRollout(packagesKey("2.0")).ID,
		} {
			r.retry(id)
		}

		for range 5 {
			r.advance(time.Minute)
			r.pass()
		}

		assert.Equal(t, 1, r.grantsTo("a"), "the host is not granted the same work again")
		assert.NotContains(t, r.leased(), "a")
		assert.True(t, r.store.host("a").HoldWanted)
		assert.Equal(t, "disk is dying; lease ended by operator", r.store.host("a").HoldReason)

		return r
	}

	play(t, false)
	sameAfterRestarts(t, play)
}

// M2: a cancelled lease that is open while its host is not held exists only
// in rows of the builds that let the hold be lifted. Ending it again puts
// the hold back, and says so in the audit log; the mark stays the first one.
func TestReview7EndingAgainPutsALiftedHoldBack(t *testing.T) {
	r, lease := mixedFleet(t, false)

	_, err := r.endLeaseID("alice", lease)
	require.NoError(t, err)

	mark := *r.storedLease(lease).Cancelled

	// What unhold did in those builds.
	require.NoError(t, r.store.SetHostHold(r.ctx, "a", false, ""))

	res, err := r.endLeaseID("bob", lease)
	require.NoError(t, err)
	assert.Contains(t, res.Message, "was already ended by alice; the host is on hold")
	assert.True(t, r.store.host("a").HoldWanted)
	assert.Equal(t, "lease ended by operator", r.store.host("a").HoldReason)
	assert.Equal(t, mark, *r.storedLease(lease).Cancelled, "the first mark stays")

	entry := r.store.lastAuditOf("action.end-lease")
	assert.Equal(t, "bob", entry.Actor)
	assert.Equal(t, map[string]any{
		keyBefore: map[string]any{keyHoldWanted: false, keyHoldReason: ""},
		keyAfter:  map[string]any{keyHoldWanted: true, keyHoldReason: "lease ended by operator"},
	}, entry.Detail["hold"])
}

// silence lets every host but the silent one sync for the given minutes.
func (r *replay) silence(silent string, minutes int) {
	r.t.Helper()

	for range minutes {
		r.now = r.now.Add(time.Minute)
		r.refreshHealth()

		for _, name := range []string{"a", "b", "c"} {
			if name != silent {
				r.sync(name, nil)
			}
		}

		r.pass()
	}
}

// L3: a lease failed because its agent was silent, and the agent, back
// again, says that it never started it. The lease stopped nothing: it is
// expired, the host is not broken by it and its member waits again. Before,
// the report was passed over and the host stayed broken by a lease under
// which nothing had happened.
func TestReview7ASilentLeaseThatWasNeverStartedIsExpired(t *testing.T) {
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

		// a holds the lease unstarted and goes silent for longer than the
		// lease's deadline and leaseTtl.
		r.silence("a", 75)

		l := r.storedLease(lease)
		require.Equal(t, fleet.LeaseFailed, l.State)
		require.Equal(t, "agent silent", l.Error)
		require.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State)
		require.Equal(t, fleet.HostFailed, r.rollout(rollout).Host("a").State)
		require.NotNil(t, newLedger([]*fleet.Lease{l}).brokenSince("a", groupPackages))

		// The agent is back: it had the lease and did not start it.
		r.reportOp("a", lease, protocol.PhaseDone, false, protocol.StepRefused)
		r.pass()

		l = r.storedLease(lease)
		require.Equal(t, fleet.LeaseExpired, l.State, "the lease stopped nothing: it did not fail")
		assert.Empty(t, l.Error)
		require.NotNil(t, l.Operation)
		require.NotNil(t, l.EndedAt)
		assert.Equal(t, r.now, *l.EndedAt)
		assert.Equal(t, protocol.StepRefused, l.Operation.Steps[0].Status, "the report is on record")
		assert.Nil(t, newLedger([]*fleet.Lease{l}).brokenSince("a", groupPackages))
		assert.Equal(t, "failed", r.store.lastAuditOf("lease.reopened").Detail["was"])
		assert.Equal(t, reasonNotDisrupted, r.store.lastAuditOf("lease.expired").Detail[keyReason])

		m := r.rollout(rollout).Host("a")
		assert.Equal(t, fleet.HostPending, m.State, "the member is not failed because of it")
		assert.Empty(t, m.LeaseID)
		assert.Empty(t, m.Error)
		assert.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State, "what halted stays halted until a person retries")
		assert.Empty(t, r.leased())

		// The agent repeats the report. Nothing more comes of it.
		entries := len(r.store.auditActions())

		r.advance(time.Minute)
		r.pass()
		assert.Equal(t, fleet.LeaseExpired, r.storedLease(lease).State)
		assert.Len(t, r.store.auditActions(), entries)

		// Retried, the host is an ordinary member: not broken, not first.
		r.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
		r.retry(rollout)
		r.pass()

		m = r.rollout(rollout).Host("a")
		assert.NotEqual(t, fleet.StageRepair, m.Stage)
		assert.NotContains(t, m.Error, brokenPrefix)

		return r
	}

	play(t, false)
	sameAfterRestarts(t, play)
}

// L3, the corners: only a lease that failed without the agent's word is
// given the end the agent reports; the sync must be from after the failure;
// and a lease the host was granted since stays its lease.
func TestReview7NeverStartedReportCorners(t *testing.T) {
	need := packagesNeed("2.0")
	refused := func() *protocol.Operation {
		return &protocol.Operation{
			LeaseID: "l-old", Phase: protocol.PhaseDone,
			Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepRefused}},
		}
	}

	tests := []struct {
		name   string
		state  fleet.LeaseState
		record *protocol.Operation
		// seen is the host's last sync, relative to the lease's end.
		seen time.Duration
		want bool
	}{
		{name: "failed as silent, the agent is back: expired", state: fleet.LeaseFailed, seen: time.Second, want: true},
		{name: "failed as silent, the sync is the one it failed on", state: fleet.LeaseFailed, seen: 0},
		{name: "failed as silent, an older sync", state: fleet.LeaseFailed, seen: -time.Second},
		{
			name: "failed with the agent's final report on record", state: fleet.LeaseFailed, seen: time.Second,
			record: &protocol.Operation{LeaseID: "l-old", Phase: protocol.PhaseFailed},
		},
		{name: "expired already", state: fleet.LeaseExpired, seen: time.Second},
		{name: "released", state: fleet.LeaseReleased, seen: time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.w.Settings.Paused = true
			b.host("a", 10)
			b.host("b", 10)

			at := b.w.Now.Add(-time.Minute)
			closed := &fleet.Lease{
				ID: "l-old", Host: "a", State: tt.state, Needs: []protocol.Need{need},
				Rollouts: map[string]string{need.ID: "id000"}, GrantedAt: at.Add(-time.Minute), EndedAt: &at,
				Operation: tt.record,
			}
			if tt.state == fleet.LeaseFailed {
				closed.Error = reasonAgentSilent
			}

			b.get("a").LastSeen = at.Add(tt.seen)
			b.w.Ended = []*fleet.Lease{closed}
			b.w.ClosedLeases = map[string]*fleet.Lease{closed.ID: closed}
			b.get("a").Status.Operation = refused()

			out := b.step()

			if !tt.want {
				assert.Empty(t, out.Leases)
				assert.Empty(t, auditActions(out))

				return
			}

			require.Len(t, out.Leases, 1)
			assert.Equal(t, fleet.LeaseExpired, out.Leases[0].State)
			assert.Empty(t, out.Leases[0].Error)
			assert.Equal(t, []string{"lease.reopened", "lease.expired"}, auditActions(out))
			assert.Empty(t, b.leasedHosts(), "an expired lease holds no slot")
		})
	}

	t.Run("the lease the host was granted since stays its lease", func(t *testing.T) {
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
		b.get("a").Status.Operation = refused()

		out := b.step()

		assert.Equal(t, []string{"lease.reopened", "lease.expired"}, auditActions(out))
		assert.Equal(t, granted, b.lease("a").ID)
		assert.Equal(t, fleet.LeaseGranted, b.lease("a").State)
		assert.Equal(t, SlotLeased, out.Plan.Hosts["a"].Slot, "the host still holds the lease it was granted")
		assert.Equal(t, granted, out.Plan.Hosts["a"].LeaseID)
		assert.Equal(t, []string{"a"}, b.leasedHosts())
		assert.Equal(t, fleet.LeaseExpired, b.w.ClosedLeases[old.ID].State)
		assert.Equal(t, 1, Reconcile(b.w).Plan.Budget.LeasedHosts, "one lease, one slot")
	})
}

// L7: resumes takes a failed lease up again only on a sync from after its
// end. The sync it failed on, at the very moment of the end, is not one.
func TestReview7ResumesNeedsASyncFromAfterTheEnd(t *testing.T) {
	at := testGenesis.Add(time.Hour)
	closed := &fleet.Lease{ID: "l", Host: "a", State: fleet.LeaseFailed, Error: reasonAgentSilent, EndedAt: &at}

	for name, tt := range map[string]struct {
		seen time.Time
		want bool
	}{
		"before the end":           {seen: at.Add(-time.Nanosecond)},
		"at the moment of the end": {seen: at},
		"after the end":            {seen: at.Add(time.Nanosecond), want: true},
		"long after":               {seen: at.Add(time.Hour), want: true},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, resumes(closed, &fleet.Host{Name: "a", LastSeen: tt.seen}))
		})
	}

	assert.False(t, resumes(closed, &fleet.Host{Name: "b", LastSeen: at.Add(time.Hour)}), "another host")

	open := *closed
	open.EndedAt = nil
	assert.False(t, resumes(&open, &fleet.Host{Name: "a", LastSeen: at.Add(time.Hour)}), "no end on record")
}

// L7: a lease that is taken up again leases only the member that is still
// failed by it. L1 fails as silent, the rollout is retried, L2 is granted and
// fails as silent too; then the agent reports L1. The member was failed by
// L2, and stays so: L1 does not take over what L2 said.
func TestReview7ALeaseTakenUpAgainLeavesTheMemberAnotherLeaseFailed(t *testing.T) {
	r := newReplay(t, false)

	for _, name := range []string{"a", "b", "c"} {
		r.addHost(name, 0)
	}

	r.reports("2.0", "a", "b", "c")
	r.pass()

	first := r.running("a")
	rollout := r.openRollout(packagesKey("2.0")).ID

	r.pass()
	r.silence("a", 35)
	require.Equal(t, "agent silent", r.storedLease(first).Error)

	// Retried; a is back without an operation and is granted again.
	r.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
	r.retry(rollout)
	r.pass()
	require.Equal(t, []string{"a"}, r.leased())

	second := r.running("a")
	require.NotEqual(t, first, second)

	r.pass()
	r.silence("a", 35)
	require.Equal(t, fleet.LeaseFailed, r.storedLease(second).State)
	require.Equal(t, second, r.rollout(rollout).Host("a").LeaseID)

	// The agent is back, and what it reports is the first lease.
	r.reportOp("a", first, protocol.PhaseApplying, true, protocol.StepRunning)
	r.pass()

	assert.Equal(t, fleet.LeaseRunning, r.storedLease(first).State, "the host is down under it")
	assert.Equal(t, fleet.LeaseFailed, r.storedLease(second).State)

	m := r.rollout(rollout).Host("a")
	assert.Equal(t, fleet.HostFailed, m.State)
	assert.Equal(t, second, m.LeaseID, "failed by the second lease, not leased by the first")
	assert.Equal(t, "agent silent", m.Error)
	assert.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State)
}

// L7: the cancel mark survives the lease being taken up again. The agent is
// told again, and its report ends the lease as ended by operator.
func TestReview7TheCancelMarkIsKeptAcrossAReopen(t *testing.T) {
	play := func(t *testing.T, restart bool) *replay {
		t.Helper()

		r, lease := mixedFleet(t, restart)

		_, err := r.endLeaseID("alice", lease)
		require.NoError(t, err)

		mark := *r.storedLease(lease).Cancelled

		r.silence("a", 35)
		require.Equal(t, "agent silent", r.storedLease(lease).Error)
		require.Equal(t, mark, *r.storedLease(lease).Cancelled)

		// The agent is back, at work.
		r.reportOp("a", lease, protocol.PhaseApplying, true, protocol.StepRunning, protocol.StepPending)
		r.pass()

		l := r.storedLease(lease)
		require.Equal(t, fleet.LeaseRunning, l.State)
		require.NotNil(t, l.Cancelled)
		assert.Equal(t, mark, *l.Cancelled)
		require.NotNil(t, r.assignment("a").Lease)
		assert.True(t, r.assignment("a").Lease.Cancel, "the agent is told again")

		_, err = r.svc.Unhold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}})
		require.ErrorIs(t, err, ErrInvalidAction, "open again: the hold stays")

		r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone, protocol.StepCancelled)
		r.pass()
		assert.Equal(t, "ended by operator", r.storedLease(lease).Error)

		return r
	}

	play(t, false)
	sameAfterRestarts(t, play)
}

// L7: a cancel beats force. A forced lease is released the moment its agent
// is done; one that a person ended is not released at all.
func TestReview7ACancelBeatsForce(t *testing.T) {
	need := packagesNeed("2.0")

	for name, cancelled := range map[string]bool{"forced": false, "forced and ended by a person": true} {
		t.Run(name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.w.Settings.Paused = true
			b.host("a", 0)
			b.host("b", 0)

			l := &fleet.Lease{
				ID: "l", Host: "a", State: fleet.LeaseRunning, Force: true, Needs: []protocol.Need{need},
				GrantedAt: b.w.Now.Add(-time.Minute), ExpiresAt: b.w.Now.Add(time.Hour),
			}
			if cancelled {
				l.Cancelled = &fleet.LeaseCancel{At: b.w.Now, By: "alice", State: fleet.LeaseRunning}
			}

			b.w.Leases = []*fleet.Lease{l}
			b.get("a").Status.Operation = &protocol.Operation{
				LeaseID: "l", Phase: protocol.PhaseDone, Disrupted: true,
				Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepDone}},
			}

			out := b.step()
			require.Len(t, out.Leases, 1)

			if !cancelled {
				assert.Equal(t, fleet.LeaseReleased, out.Leases[0].State)

				return
			}

			assert.Equal(t, fleet.LeaseFailed, out.Leases[0].State)
			assert.Equal(t, reasonEndedByOperator, out.Leases[0].Error)
			assert.Equal(t, verdictBroke, verdictOf(out.Leases[0], groupPackages))
		})
	}
}

// L7: a cancelled lease is restated, with the mark, while it is running and
// while it is verifying; only a granted one that the agent has not reported
// an operation for is withheld.
func TestReview7CancelledLeasesAreRestated(t *testing.T) {
	for _, state := range []fleet.LeaseState{fleet.LeaseGranted, fleet.LeaseRunning, fleet.LeaseVerifying} {
		t.Run(string(state), func(t *testing.T) {
			r := newReplay(t, false)

			for _, name := range []string{"a", "b", "c"} {
				r.addHost(name, 0)
			}

			r.reports("2.0", "a", "b", "c")
			r.pass()

			lease := r.assignment("a").Lease.ID

			switch state {
			case fleet.LeaseRunning:
				r.running("a")
				r.pass()
			case fleet.LeaseVerifying:
				r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone)
				r.pass()
			default:
			}

			require.Equal(t, state, r.storedLease(lease).State)

			_, err := r.endLeaseID("alice", lease)
			require.NoError(t, err)

			offered := r.assignment("a").Lease

			if state == fleet.LeaseGranted {
				assert.Nil(t, offered, "not started: no longer offered")

				return
			}

			require.NotNil(t, offered)
			assert.Equal(t, lease, offered.ID)
			assert.True(t, offered.Cancel)
			assert.NotEmpty(t, offered.Needs)

			// Only a granted lease is withheld from an agent that reports no
			// operation for it. One that has been started is restated whatever
			// the agent's last sync carried.
			r.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })

			offered = r.assignment("a").Lease
			require.NotNil(t, offered, "a started lease is restated, with or without a report")
			assert.Equal(t, lease, offered.ID)
			assert.True(t, offered.Cancel)
		})
	}
}

// M1 of the review of the seventh round: a lease that a person ended is
// never open on a host that is not held. The lease ends without its agent's
// word (the agent is silent); the hold may be lifted then, and is. When the
// agent reports the operation after all, the lease is taken up again with
// its mark, and the hold is put back in the same commit: otherwise the host
// would be granted the same work the moment the lease ends again.
func TestReview7ACancelledLeaseTakenUpAgainHoldsItsHostAgain(t *testing.T) {
	for _, back := range []string{"at work", "with the final report"} {
		play := func(t *testing.T, restart bool) *replay {
			t.Helper()

			r, lease := mixedFleet(t, restart)
			ri := r.openRollout(imageKey(imgLighthouse, digestA)).ID
			rp := r.openRollout(packagesKey("2.0")).ID

			_, err := r.endLeaseID("alice", lease)
			require.NoError(t, err)

			r.silence("a", 35)
			require.Equal(t, "agent silent", r.storedLease(lease).Error)

			// The lease has ended: the hold can be lifted, and is.
			r.fresh()

			_, err = r.svc.Unhold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}})
			require.NoError(t, err)
			require.False(t, r.store.host("a").HoldWanted)
			r.dump()

			if back == "at work" {
				// The agent is back, bringing its containers up.
				r.reportOp("a", lease, protocol.PhaseStarting, true, protocol.StepDone, protocol.StepCancelled)
				r.pass()

				l := r.storedLease(lease)
				require.Equal(t, fleet.LeaseRunning, l.State)
				require.NotNil(t, l.Cancelled)
				assert.True(t, r.store.host("a").HoldWanted, "open and cancelled: held")
				assert.Equal(t, "lease ended by operator", r.store.host("a").HoldReason)
				assert.True(t, r.assignment("a").Lease.Cancel)

				r.fresh()

				_, err = r.svc.Unhold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}})
				require.ErrorIs(t, err, ErrInvalidAction, "open again: the hold stays")
				r.dump()
			}

			r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone, protocol.StepCancelled)
			r.pass()

			l := r.storedLease(lease)
			assert.Equal(t, fleet.LeaseFailed, l.State)
			assert.Equal(t, "ended by operator", l.Error)
			assert.True(t, r.store.host("a").HoldWanted, "the end of the lease does not lift the hold")
			assert.Equal(t, "lease ended by operator", r.store.host("a").HoldReason)

			entry := r.store.lastAuditOf("host.held")
			assert.Equal(t, actorScheduler, entry.Actor)
			assert.Equal(t, "a", entry.Target)
			assert.Equal(t, lease, entry.Detail[keyLease])
			assert.Equal(t, map[string]any{keyHoldWanted: false, keyHoldReason: ""}, entry.Detail[keyBefore])
			assert.Equal(t, map[string]any{keyHoldWanted: true, keyHoldReason: "lease ended by operator"},
				entry.Detail[keyAfter])

			// Retried, nobody is granted on that host: its agent reports the
			// needs it had, and it is held.
			r.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
			r.retry(ri)
			r.retry(rp)

			for range 5 {
				r.advance(time.Minute)
				r.pass()
			}

			assert.Equal(t, 1, r.grantsTo("a"), "the host is not granted the same work again")
			assert.NotContains(t, r.leased(), "a")

			return r
		}

		t.Run(back, func(t *testing.T) {
			play(t, false)
			sameAfterRestarts(t, play)
		})
	}
}

// The corners of that hold: an expired lease with the mark that its agent
// started after all is held again too; a hold that somebody set meanwhile
// stays, with the cancel in its reason, once; and a lease the agent says it
// never started holds nobody (it is not open after the pass, and the person
// who lifted the hold did so after it had ended).
func TestReview7HoldOfACancelledLeaseTakenUpAgainCorners(t *testing.T) {
	need := packagesNeed("2.0")
	working := &protocol.Operation{
		LeaseID: "l-old", Phase: protocol.PhaseApplying, Disrupted: true,
		Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepRunning}},
	}
	declined := &protocol.Operation{
		LeaseID: "l-old", Phase: protocol.PhaseDone,
		Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepRefused}},
	}
	// At work, and nothing stopped yet: the lease is open all the same.
	begun := &protocol.Operation{
		LeaseID: "l-old", Phase: protocol.PhaseApplying,
		Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepPending}},
	}

	tests := []struct {
		name      string
		state     fleet.LeaseState
		cancelled bool
		held      bool
		reason    string
		op        *protocol.Operation
		want      string
	}{
		{name: "expired, started after all", state: fleet.LeaseExpired, cancelled: true, op: working, want: holdReasonEnded},
		{name: "failed as silent, at work", state: fleet.LeaseFailed, cancelled: true, op: working, want: holdReasonEnded},
		{
			name: "held meanwhile for another reason", state: fleet.LeaseFailed, cancelled: true, op: working,
			held: true, reason: "disk is dying", want: "disk is dying; " + holdReasonEnded,
		},
		{
			name: "still held for the cancel: nothing to write", state: fleet.LeaseFailed, cancelled: true, op: working,
			held: true, reason: "disk is dying; " + holdReasonEnded,
		},
		{
			name:  "failed as silent, at work and nothing disrupted yet: open, so held",
			state: fleet.LeaseFailed, cancelled: true, op: begun, want: holdReasonEnded,
		},
		{name: "never started: nobody is held", state: fleet.LeaseFailed, cancelled: true, op: declined},
		{name: "not cancelled: nobody is held", state: fleet.LeaseFailed, op: working},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.w.Settings.Paused = true
			b.host("a", 10)
			b.host("b", 10)
			b.get("a").HoldWanted, b.get("a").HoldReason = tt.held, tt.reason

			at := b.w.Now.Add(-time.Minute)
			closed := &fleet.Lease{
				ID: "l-old", Host: "a", State: tt.state, Needs: []protocol.Need{need},
				Rollouts: map[string]string{need.ID: "id000"}, GrantedAt: at.Add(-time.Minute), EndedAt: &at,
			}
			if tt.state == fleet.LeaseFailed {
				closed.Error = reasonAgentSilent
			}

			if tt.cancelled {
				closed.Cancelled = &fleet.LeaseCancel{At: at.Add(-time.Second), By: "alice", State: fleet.LeaseRunning}
			}

			b.w.Ended = []*fleet.Lease{closed}
			b.w.ClosedLeases = map[string]*fleet.Lease{closed.ID: closed}
			b.get("a").Status.Operation = tt.op

			out := b.step()
			require.Contains(t, auditActions(out), "lease.reopened")

			if tt.want == "" {
				assert.Empty(t, out.Holds)
				assert.NotContains(t, auditActions(out), "host.held")

				return
			}

			require.Len(t, out.Leases, 1)
			assert.Equal(t, fleet.LeaseRunning, out.Leases[0].State)
			assert.NotNil(t, out.Leases[0].Cancelled)
			assert.Equal(t, []store.HostHold{{Host: "a", Wanted: true, Reason: tt.want}}, out.Holds)
			assert.Contains(t, auditActions(out), "host.held")
			assert.Equal(t, out.Holds, out.Changes().Holds, "written in the transaction of the lease")
			assert.Equal(t, SlotLeased, out.Plan.Hosts["a"].Slot)

			// The next pass finds the host held and has nothing to add.
			again := b.step()
			assert.Empty(t, again.Holds)
		})
	}
}

// L2 of the review of the seventh round: under a lease that a person ended,
// a group is judged by whether anything of it was applied. Two containers of
// one image reference (the beacon node and the validator client): the first
// was recreated, the second never begun. Something was applied and the host
// was never shown to be back: broken. With nothing of the group done, the
// end says nothing; and the packages, never begun, are spared either way.
func TestReview7EndedLeaseBreaksForAGroupOfWhichAnythingWasApplied(t *testing.T) {
	bn, vc := imageNeed("bn", imgLighthouse, digestA), imageNeed("vc", imgLighthouse, digestA)
	other := packagesNeed("2.0")
	group := prefixImageKey + imgLighthouse

	done, cancelled, refused, superseded, failed := protocol.StepDone, protocol.StepCancelled,
		protocol.StepRefused, protocol.StepSuperseded, protocol.StepFailed

	tests := []struct {
		name     string
		statuses [3]protocol.StepStatus
		// want: the verdict for the image reference and for the packages.
		want [2]leaseVerdict
	}{
		{"bn done, vc cancelled", [3]protocol.StepStatus{done, cancelled, cancelled}, [2]leaseVerdict{verdictBroke, verdictNone}},
		{"bn cancelled, vc done", [3]protocol.StepStatus{cancelled, done, cancelled}, [2]leaseVerdict{verdictBroke, verdictNone}},
		{"both done", [3]protocol.StepStatus{done, done, cancelled}, [2]leaseVerdict{verdictBroke, verdictNone}},
		{"bn done, vc superseded", [3]protocol.StepStatus{done, superseded, done}, [2]leaseVerdict{verdictBroke, verdictBroke}},
		{"neither begun", [3]protocol.StepStatus{cancelled, cancelled, done}, [2]leaseVerdict{verdictNone, verdictBroke}},
		{"refused and superseded", [3]protocol.StepStatus{refused, superseded, cancelled}, [2]leaseVerdict{verdictNone, verdictNone}},
		{"bn failed, vc cancelled", [3]protocol.StepStatus{failed, cancelled, cancelled}, [2]leaseVerdict{verdictBroke, verdictNone}},
	}

	for _, tt := range tests {
		for _, state := range []fleet.LeaseState{fleet.LeaseGranted, fleet.LeaseRunning, fleet.LeaseVerifying} {
			t.Run(tt.name+", ended while "+string(state), func(t *testing.T) {
				l := &fleet.Lease{
					ID: "l", Host: "a", State: fleet.LeaseFailed, Error: reasonEndedByOperator,
					Cancelled: &fleet.LeaseCancel{By: "alice", State: state},
					Needs:     []protocol.Need{bn, vc, other},
					Operation: &protocol.Operation{LeaseID: "l", Phase: protocol.PhaseDone, Disrupted: true},
				}

				for i, n := range l.Needs {
					l.Operation.Steps = append(l.Operation.Steps, protocol.Step{NeedID: n.ID, Status: tt.statuses[i]})
				}

				assert.Equal(t, tt.want[0], verdictOf(l, group), "image")
				assert.Equal(t, tt.want[1], verdictOf(l, groupPackages), "packages")
			})
		}
	}
}

// L5: the reason of a hold names the cancel at most once, whatever was there
// before and however often a lease of the host is ended.
func TestReview7TheHoldReasonNamesTheCancelOnce(t *testing.T) {
	for _, tt := range []struct {
		held   bool
		reason string
		want   string
	}{
		{false, "", holdReasonEnded},
		{false, "a reason from an earlier hold", holdReasonEnded},
		{true, "", holdReasonEnded},
		{true, "disk is dying", "disk is dying; " + holdReasonEnded},
		{true, holdReasonEnded, holdReasonEnded},
		{true, "disk is dying; " + holdReasonEnded, "disk is dying; " + holdReasonEnded},
		{true, holdReasonEnded + "; and the disk is dying", holdReasonEnded + "; and the disk is dying"},
	} {
		h := &fleet.Host{Name: "a", HoldWanted: tt.held, HoldReason: tt.reason}
		got := endedHoldReason(h)
		assert.Equal(t, tt.want, got, "held: %v, reason %q", tt.held, tt.reason)

		// Ended again under the hold it left: nothing is added.
		assert.Equal(t, got, endedHoldReason(&fleet.Host{Name: "a", HoldWanted: true, HoldReason: got}))
	}

	// Two leases of one host ended one after the other, through the action.
	r, lease := mixedFleet(t, false)

	_, err := r.svc.Hold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}, Reason: "disk is dying"})
	require.NoError(t, err)

	_, err = r.endLeaseID("alice", lease)
	require.NoError(t, err)

	r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepCancelled, protocol.StepCancelled)
	r.pass()
	require.Equal(t, fleet.LeaseFailed, r.storedLease(lease).State)

	// A second lease of the host, written as an adopted one would be, ended
	// under the same hold.
	second := &fleet.Lease{
		ID: "second", Host: "a", State: fleet.LeaseRunning, Needs: []protocol.Need{packagesNeed("2.0")},
		GrantedBy: grantedByAdoption, GrantedAt: r.now, ExpiresAt: r.now.Add(time.Hour),
	}
	require.NoError(t, r.store.Commit(r.ctx, &store.Changes{Leases: []*fleet.Lease{second}}))

	_, err = r.endLeaseID("alice", second.ID)
	require.NoError(t, err)
	assert.Equal(t, "disk is dying; lease ended by operator", r.store.host("a").HoldReason)
}

// L5: only a lease that a person ended keeps the hold of its host from being
// lifted. A host that is held while it works under an ordinary lease can be
// unheld at any time.
func TestReview7UnholdIsRefusedOnlyForACancelledLease(t *testing.T) {
	for _, state := range []fleet.LeaseState{fleet.LeaseGranted, fleet.LeaseRunning, fleet.LeaseVerifying} {
		t.Run(string(state), func(t *testing.T) {
			r := newReplay(t, false)

			for _, name := range []string{"a", "b", "c"} {
				r.addHost(name, 0)
			}

			r.reports("2.0", "a", "b", "c")
			r.pass()

			lease := r.assignment("a").Lease.ID

			switch state {
			case fleet.LeaseRunning:
				r.running("a")
				r.pass()
			case fleet.LeaseVerifying:
				r.reportOp("a", lease, protocol.PhaseDone, true, protocol.StepDone)
				r.pass()
			default:
			}

			require.Equal(t, state, r.storedLease(lease).State)

			_, err := r.svc.Hold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}, Reason: "looking at it"})
			require.NoError(t, err)

			_, err = r.svc.Unhold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}})
			require.NoError(t, err, "an open lease that nobody ended does not keep the hold")
			assert.False(t, r.store.host("a").HoldWanted)
			assert.Equal(t, state, r.storedLease(lease).State)

			// Held again and the lease ended: now the hold stays.
			_, err = r.svc.Hold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}, Reason: "looking at it"})
			require.NoError(t, err)

			_, err = r.endLeaseID("alice", lease)
			require.NoError(t, err)

			_, err = r.svc.Unhold(r.ctx, "carol", HoldRequest{Hosts: []string{"a"}})
			require.ErrorIs(t, err, ErrInvalidAction)
			assert.True(t, r.store.host("a").HoldWanted)
		})
	}
}

// A lease that is taken up again serves a rollout again only where its
// member waited or was still failed by it. A member that a person skipped
// before the agent came back stays skipped: the lease is not its lease any
// more, and when it is released it is not the rollout's first success. The
// rollout still sends one canary alone. (Found by the simulation, seed 8303:
// there the oracle counted the release as a success.)
func TestReview7AReopenedLeaseDoesNotServeAMemberThatWasSkipped(t *testing.T) {
	play := func(t *testing.T, restart bool) *replay {
		t.Helper()

		r := newReplay(t, restart)

		names := make([]string, 0, 20)
		for i := range 20 {
			names = append(names, fmt.Sprintf("h%02d", i))
			r.addHost(names[i], 0)
		}

		r.reports("2.0", names...)
		r.pass()
		require.Equal(t, []string{"h00"}, r.leased(), "the canary, alone")

		lease := r.running("h00")
		rollout := r.openRollout(packagesKey("2.0")).ID

		r.pass()

		// h00 goes silent for longer than leaseTtl; its lease fails.
		for range 35 {
			r.now = r.now.Add(time.Minute)
			r.refreshHealth()

			for _, name := range names[1:] {
				r.sync(name, nil)
			}

			r.pass()
		}

		require.Equal(t, "agent silent", r.storedLease(lease).Error)
		require.Equal(t, fleet.RolloutHalted, r.rollout(rollout).State)

		// A person skips the host and lets the rollout go on.
		r.skip(rollout, "h00")
		r.retry(rollout)

		// The agent is back, at work: the lease is taken up again.
		r.reportOp("h00", lease, protocol.PhaseApplying, true, protocol.StepRunning)
		r.pass()

		require.Equal(t, fleet.LeaseRunning, r.storedLease(lease).State)

		m := r.rollout(rollout).Host("h00")
		assert.Equal(t, fleet.HostSkipped, m.State, "skipped before the lease was taken up: not leased again")
		assert.NotEqual(t, fleet.HostLeased, m.State)

		// It finishes and is released. That is no success of the rollout.
		r.done("h00")
		require.Equal(t, fleet.LeaseReleased, r.storedLease(lease).State)
		assert.Equal(t, fleet.HostSkipped, r.rollout(rollout).Host("h00").State)

		for _, member := range r.rollout(rollout).Hosts {
			assert.False(t, member.State == fleet.HostDone && member.LeaseID != "",
				"%s is done through a lease: the rollout has had no success yet", member.Host)
		}

		// So one canary goes alone, although two hosts may hold a lease.
		for range 3 {
			r.advance(time.Minute)
			r.pass()
		}

		plan := r.svc.Plan()
		require.Equal(t, 2, plan.Budget.MaxLeasedHosts)
		assert.Len(t, r.leased(), 1, "one canary, alone: the released lease was no first success")

		canary := r.rollout(rollout).Canary
		assert.Equal(t, []string{canary}, r.leased())

		waiting := 0

		for host, reason := range queueReasons(plan) {
			if reason == reasonAfterCanary+canary {
				waiting++
			} else {
				assert.Equal(t, "h00", host, "%s waits for %q", host, reason)
			}
		}

		assert.Equal(t, 18, waiting, "everybody else waits for the canary")

		return r
	}

	play(t, false)
	sameAfterRestarts(t, play)
}
