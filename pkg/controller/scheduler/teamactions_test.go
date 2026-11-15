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
	r.svc.hist = history{}

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
				"c has neither failed nor been skipped in the rollout; "+
				"ghost has neither failed nor been skipped in the rollout")
			assert.Empty(t, res.Hosts)
			assert.Equal(t, before, r.rollout(id))
			assert.Len(t, r.store.auditActions(), audits)

			// One of the two failed hosts: it waits again, the rollout stays
			// halted for the other.
			res, err = retry("a", "c")
			require.NoError(t, err)
			assert.Equal(t, []string{"a"}, res.Hosts)
			assert.Equal(t, map[string]string{"c": "has neither failed nor been skipped in the rollout"},
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

// TestRetryOfAnAbortedRolloutTakesNoHosts: reopening a cause is about every
// host that has it. A request that names hosts is refused and opens nothing.
func TestRetryOfAnAbortedRolloutTakesNoHosts(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run("restart="+strconv.FormatBool(restart), func(t *testing.T) {
			r := newReplay(t, restart)
			r.addHost("a", 0, packagesNeed("2.0"))
			r.addHost("b", 0, packagesNeed("2.0"))
			r.tick()

			id := r.openRollouts()[0].ID
			r.abort(id)
			r.pass()
			require.Empty(t, r.openRollouts())

			audits := len(r.store.auditActions())

			for _, hosts := range [][]string{{"a"}, {"ghost"}, {"a", "b"}} {
				r.fresh()

				_, err := r.svc.Retry(r.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: hosts})
				require.ErrorIs(t, err, ErrInvalidAction)
				require.ErrorContains(t, err, "is aborted; retrying it reopens its cause for every host")
			}

			r.pass()
			assert.Empty(t, r.openRollouts(), "the cause stays blocked")
			assert.Len(t, r.store.auditActions(), audits)
			assert.Equal(t, fleet.RolloutAborted, r.rollout(id).State)

			// Without hosts it is reopened, as before.
			r.fresh()

			res, err := r.svc.Retry(r.ctx, "alice", RolloutHostsRequest{Rollout: id})
			require.NoError(t, err)
			assert.NotEqual(t, id, res.Rollout)
			assert.Len(t, r.openRollouts(), 1)
		})
	}
}

func asTeam(ctx context.Context) context.Context {
	return WithAuthorization(ctx, Authorization{By: "team_hold", Teams: []string{"lighthouse"}})
}

func asStaff(ctx context.Context) context.Context {
	return WithAuthorization(ctx, Authorization{By: "hold", Teams: []string{"panda"}})
}

// TestHoldRemembersWhoSetIt: a hold set through the team feature is a
// team's; a hold of staff stays staff's when a team sets it again; every
// other writer of a hold makes it staff's.
func TestHoldRemembersWhoSetIt(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.tick()

	hold := func(ctx context.Context, reason string) {
		_, err := h.svc.Hold(ctx, "alice", HoldRequest{Hosts: []string{"a"}, Reason: reason})
		require.NoError(t, err)
	}
	unhold := func(ctx context.Context) {
		_, err := h.svc.Unhold(ctx, "alice", HoldRequest{Hosts: []string{"a"}})
		require.NoError(t, err)
	}
	by := func() string { return h.store.host("a").HoldBy }

	hold(asTeam(h.ctx), "ours")
	assert.True(t, h.store.host("a").HoldWanted)
	assert.Equal(t, fleet.HoldByTeam, by())

	entry := h.store.lastAuditOf("action.hold")
	assert.Equal(t, map[string]any{keyHoldWanted: true, keyHoldReason: "ours", keyHoldBy: fleet.HoldByTeam},
		entry.Detail[keyAfter])
	assert.Equal(t, "team_hold", entry.Detail[keyAuthorizedBy])

	// The team again: still the team's.
	hold(asTeam(h.ctx), "ours, still")
	assert.Equal(t, fleet.HoldByTeam, by())

	// Staff over it: staff's, and the entry says that it was a team's.
	hold(asStaff(h.ctx), "staff")
	assert.Empty(t, by())

	entry = h.store.lastAuditOf("action.hold")
	assert.Equal(t, map[string]any{keyHoldWanted: true, keyHoldReason: "ours, still", keyHoldBy: fleet.HoldByTeam},
		entry.Detail[keyBefore])
	assert.Equal(t, map[string]any{keyHoldWanted: true, keyHoldReason: "staff", keyHoldBy: holdByStaff},
		entry.Detail[keyAfter])

	// A team cannot make it its own by setting it again, nor lift it: the
	// scheduler asks again under its lock what the API asked before.
	audits := len(h.store.auditActions())

	_, err := h.svc.Hold(asTeam(h.ctx), "alice", HoldRequest{Hosts: []string{"a"}, Reason: "ours now?"})
	require.ErrorIs(t, err, ErrForbidden)
	require.ErrorContains(t, err, "team_hold does not change the hold on host a (set by staff)")

	_, err = h.svc.Unhold(asTeam(h.ctx), "alice", HoldRequest{Hosts: []string{"a"}})
	require.ErrorIs(t, err, ErrForbidden)
	require.ErrorContains(t, err, "team_hold does not lift the hold on host a (set by staff)")

	// Also with admin next to the twin: what allowed the hold is the twin.
	_, err = h.svc.Unhold(WithAuthorization(h.ctx, Authorization{By: "team_hold,admin"}), "alice",
		HoldRequest{Hosts: []string{"a"}})
	require.ErrorIs(t, err, ErrForbidden)

	assert.True(t, h.store.host("a").HoldWanted)
	assert.Equal(t, "staff", h.store.host("a").HoldReason)
	assert.Empty(t, by())
	assert.Len(t, h.store.auditActions(), audits, "a refusal changes and records nothing")

	unhold(asStaff(h.ctx))
	assert.False(t, h.store.host("a").HoldWanted)
	assert.Empty(t, by())

	// Without an authorization (a caller that is not the API): staff's.
	hold(h.ctx, "plain")
	assert.Empty(t, by())
	unhold(h.ctx)

	// A request allowed by the plain feature of a login that also is in
	// teams: staff's. The feature decides, not the teams.
	hold(WithAuthorization(h.ctx, Authorization{By: "hold", Teams: []string{"lighthouse"}}), "x")
	assert.Empty(t, by())
	unhold(h.ctx)

	// The end of a lease takes a team's hold over: it extends the reason
	// through the commit of the cancel, which is staff's.
	hold(asTeam(h.ctx), "ours")
	require.Equal(t, fleet.HoldByTeam, by())

	lease := openLeaseOf(t, h, "a")
	_, err = h.svc.EndLease(h.ctx, "bob", EndLeaseRequest{Lease: lease})
	require.NoError(t, err)
	assert.True(t, h.store.host("a").HoldWanted)
	assert.Empty(t, by(), "ended by a person: staff's hold from here on")

	// It survives a restart, like the reason does.
	h.restart()
	h.tick()
	assert.Empty(t, by())
}

// TestEndLeaseMakesTheHoldStaffsWhateverItsReason: a team that holds its host
// with the very reason the end of a lease writes does not keep the hold. The
// decision is made on whose hold it is, not on its text.
func TestEndLeaseMakesTheHoldStaffsWhateverItsReason(t *testing.T) {
	for _, reason := range []string{
		holdReasonEnded, "x; " + holdReasonEnded, holdReasonEnded + "; " + holdReasonEnded, "", "ours",
	} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t)
			h.addHost("a", 0, packagesNeed("2.0"))
			h.tick()

			lease := openLeaseOf(t, h, "a")

			_, err := h.svc.Hold(asTeam(h.ctx), "alice", HoldRequest{Hosts: []string{"a"}, Reason: reason})
			require.NoError(t, err)
			require.Equal(t, fleet.HoldByTeam, h.store.host("a").HoldBy)

			_, err = h.svc.EndLease(WithAuthorization(h.ctx, Authorization{By: "team_recover"}), "alice",
				EndLeaseRequest{Lease: lease})
			require.NoError(t, err)

			host := h.store.host("a")
			assert.True(t, host.HoldWanted)
			assert.Empty(t, host.HoldBy, "the hold of an ended lease is staff's")
			assert.Contains(t, host.HoldReason, holdReasonEnded)

			entry := h.store.lastAuditOf("action.end-lease")
			hold, ok := entry.Detail["hold"].(map[string]any)
			require.True(t, ok, "the change of the hold is audited")
			assert.Equal(t, map[string]any{keyHoldWanted: true, keyHoldReason: reason, keyHoldBy: fleet.HoldByTeam},
				hold[keyBefore])
			assert.Equal(t, map[string]any{
				keyHoldWanted: true, keyHoldReason: host.HoldReason, keyHoldBy: holdByStaff,
			}, hold[keyAfter])

			// The team cannot lift it, while the lease is open or after.
			_, err = h.svc.Unhold(asTeam(h.ctx), "alice", HoldRequest{Hosts: []string{"a"}})
			require.ErrorIs(t, err, ErrForbidden)

			h.restart()
			h.tick()

			_, err = h.svc.Unhold(asTeam(h.ctx), "alice", HoldRequest{Hosts: []string{"a"}})
			require.ErrorIs(t, err, ErrForbidden)
			assert.True(t, h.store.host("a").HoldWanted)

			// Ending it again where the hold is a team's (a row of the build
			// before this rule) puts a staff hold back as well.
			h2 := newHarness(t)
			h2.addHost("a", 0, packagesNeed("2.0"))
			h2.tick()

			again := openLeaseOf(t, h2, "a")
			_, err = h2.svc.EndLease(h2.ctx, "bob", EndLeaseRequest{Lease: again})
			require.NoError(t, err)
			require.NoError(t, h2.store.Commit(h2.ctx, &store.Changes{Holds: []store.HostHold{
				{Host: "a", Wanted: true, Reason: reason, By: fleet.HoldByTeam},
			}}))

			_, err = h2.svc.EndLease(h2.ctx, "bob", EndLeaseRequest{Lease: again})
			require.NoError(t, err)
			assert.Empty(t, h2.store.host("a").HoldBy)
		})
	}
}

// TestHoldPutBackOnReopenIsStaffs: a lease that a person ended is taken up
// again after its hold was lifted. The scheduler puts the hold back, as
// staff's, also where a team holds the host by then, with any reason.
func TestHoldPutBackOnReopenIsStaffs(t *testing.T) {
	for _, reason := range []string{holdReasonEnded, "ours"} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t)
			h.addHost("a", 0, packagesNeed("2.0"))
			h.tick()

			lease := h.running("a")

			_, err := h.svc.EndLease(h.ctx, "bob", EndLeaseRequest{Lease: lease})
			require.NoError(t, err)

			// The agent is silent until the lease has failed without its word.
			for range 40 {
				h.now = h.now.Add(time.Minute)
				h.refreshHealth()
				h.tick()
			}

			leases, err := h.store.ListLeases(h.ctx, store.LeaseFilter{OpenOnly: true, Host: "a"})
			require.NoError(t, err)
			require.Empty(t, leases, "the lease has ended")

			// Staff lifts the hold; a team member holds the host again.
			_, err = h.svc.Unhold(h.ctx, "bob", HoldRequest{Hosts: []string{"a"}})
			require.NoError(t, err)
			_, err = h.svc.Hold(asTeam(h.ctx), "alice", HoldRequest{Hosts: []string{"a"}, Reason: reason})
			require.NoError(t, err)
			require.Equal(t, fleet.HoldByTeam, h.store.host("a").HoldBy)

			// The agent is back, at work under the lease.
			h.sync("a", nil)
			h.tick()
			require.Equal(t, "lease.reopened", h.store.lastAuditOf("lease.reopened").Action)

			host := h.store.host("a")
			assert.True(t, host.HoldWanted)
			assert.Empty(t, host.HoldBy, "put back as staff's")

			entry := h.store.lastAuditOf("host.held")
			require.Equal(t, "host.held", entry.Action)
			assert.Equal(t, map[string]any{keyHoldWanted: true, keyHoldReason: reason, keyHoldBy: fleet.HoldByTeam},
				entry.Detail[keyBefore])
			assert.Equal(t, map[string]any{
				keyHoldWanted: true, keyHoldReason: host.HoldReason, keyHoldBy: holdByStaff,
			}, entry.Detail[keyAfter])

			_, err = h.svc.Unhold(asTeam(h.ctx), "alice", HoldRequest{Hosts: []string{"a"}})
			require.ErrorIs(t, err, ErrForbidden)
		})
	}
}

// TestHoldUndoKeepsWhoseItWas: a change of a hold that cannot be audited is
// taken back, with whose hold it was.
func TestHoldUndoKeepsWhoseItWas(t *testing.T) {
	h := newHarness(t)
	h.addHost("a", 0)

	_, err := h.svc.Hold(asTeam(h.ctx), "alice", HoldRequest{Hosts: []string{"a"}, Reason: "ours"})
	require.NoError(t, err)

	// The hold is written, its audit entry fails, the undo is written.
	h.store.failSkip, h.store.failNext = 1, 1

	_, err = h.svc.Hold(asStaff(h.ctx), "bob", HoldRequest{Hosts: []string{"a"}, Reason: "staff"})
	require.ErrorIs(t, err, errWrite)

	h.store.failSkip, h.store.failNext = 1, 1

	_, err = h.svc.Unhold(asStaff(h.ctx), "bob", HoldRequest{Hosts: []string{"a"}})
	require.ErrorIs(t, err, errWrite)

	host := h.store.host("a")
	assert.True(t, host.HoldWanted)
	assert.Equal(t, "ours", host.HoldReason)
	assert.Equal(t, fleet.HoldByTeam, host.HoldBy, "the undo restores the team's hold")
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
	assert.Equal(t, "hold lifted on 0 host(s); 1 host(s) were not held", res.Message)
	assert.Equal(t, audits, h.store.auditActions())

	// Through the team feature as well: there is no hold to refuse.
	res, err = h.svc.Unhold(asTeam(h.ctx), "alice", HoldRequest{Hosts: []string{"a"}})
	require.NoError(t, err)
	assert.Empty(t, res.Hosts)
	assert.Equal(t, audits, h.store.auditActions())

	res, err = h.svc.Unhold(h.ctx, "alice", HoldRequest{Hosts: []string{"a", "b"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, res.Hosts)
	assert.Equal(t, "hold lifted on 1 host(s); 1 host(s) were not held", res.Message)
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

	_, err := h.svc.Pause(as("supervise"), "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	recorded("action.pause", "supervise")

	_, err = h.svc.Resume(as("supervise"), "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	recorded("action.resume", "supervise")

	h.failLease("a")

	_, err = h.svc.Retry(as("team_recover"), "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"a"}})
	require.NoError(t, err)
	recorded("action.retry", "team_recover")

	_, err = h.svc.Skip(as("team_recover"), "alice", RolloutHostsRequest{Rollout: id, Hosts: []string{"c"}})
	require.NoError(t, err)
	recorded("action.skip", "team_recover")

	h.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
	h.tick()

	lease := openLeaseOf(t, h, "")
	_, err = h.svc.EndLease(as("recover"), "alice", EndLeaseRequest{Lease: lease})
	require.NoError(t, err)
	recorded("action.end-lease", "recover")

	_, err = h.svc.Abort(as("supervise"), "alice", ScopeRequest{Rollout: id})
	require.NoError(t, err)
	recorded("action.abort", "supervise")

	_, err = h.svc.Roll(as("team_roll"), "alice", RollRequest{Hosts: []string{"b"}, Staggered: true})
	require.NoError(t, err)
	recorded("action.roll", "team_roll")

	_, err = h.svc.Reboot(as("reboot,admin"), "alice", RebootRequest{Hosts: []string{"b"}, Force: true})
	require.NoError(t, err)
	recorded("action.reboot", "reboot,admin")

	_, err = h.svc.Pause(as("admin"), "alice", ScopeRequest{})
	require.NoError(t, err)
	recorded("action.pause", "admin")

	settings, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)

	settings.MinPeers = 7
	require.NoError(t, h.svc.UpdateSettings(as("admin"), "alice", settings))
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
