package scheduler

import (
	"encoding/json"
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

// The tests in this file replay the scenarios of the fifth review of the
// scheduler. They were found in what rollouts handed to each other: which
// rollout a new one "follows" was decided by when rollouts were created, and
// that picked the wrong one whenever a younger rollout of a group had closed
// before an older one. Nothing is handed on any more: who is broken and which
// lease is in flight is read off the lease table (broken.go), so the cases
// have no place left to go wrong in. Each test names what it showed before.

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

func (r *replay) abort(id string) {
	r.t.Helper()
	r.fresh()

	_, err := r.svc.Abort(r.ctx, "alice", ScopeRequest{Rollout: id, Reason: "stop"})
	require.NoError(r.t, err)
	r.dump()
}

// retry retries the rollout and returns the rollout the action names.
func (r *replay) retry(id string, hosts ...string) string {
	r.t.Helper()
	r.fresh()

	res, err := r.svc.Retry(r.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: hosts, Reason: "again"})
	require.NoError(r.t, err)
	r.dump()

	return res.Rollout
}

func (r *replay) skip(id string, hosts ...string) {
	r.t.Helper()
	r.fresh()

	_, err := r.svc.Skip(r.ctx, "alice", RolloutHostsRequest{Rollout: id, Hosts: hosts, Reason: "broken"})
	require.NoError(r.t, err)
	r.dump()
}

func (r *replay) hold(host string) {
	r.t.Helper()
	r.fresh()

	_, err := r.svc.Hold(r.ctx, "alice", HoldRequest{Hosts: []string{host}, Reason: "broken"})
	require.NoError(r.t, err)
	r.sync(host, func(st *protocol.SyncRequest) {
		st.Hold.Controller = true
		st.Needs = nil
	})
	r.dump()
}

// endLease ends the host's open lease as a person does.
func (r *replay) endLease(host string) (*ActionResult, error) {
	r.t.Helper()
	r.fresh()

	lease := r.assignment(host).Lease
	require.NotNil(r.t, lease, "no lease on %s", host)

	res, err := r.svc.EndLease(r.ctx, "alice", EndLeaseRequest{Lease: lease.ID, Reason: "taken away"})
	r.dump()

	return res, err
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

// fail reports the host's lease failed.
func (r *replay) fail(host string) {
	r.t.Helper()

	lease := r.assignment(host).Lease
	require.NotNil(r.t, lease, "no lease on %s", host)

	r.sync(host, func(st *protocol.SyncRequest) {
		st.Operation = &protocol.Operation{
			LeaseID: lease.ID, Phase: protocol.PhaseFailed, Disrupted: true, Error: "container did not start",
			Steps: []protocol.Step{{NeedID: lease.Needs[0].ID, Status: protocol.StepFailed}},
		}
	})
	r.pass()
}

// repairStage names the members of a rollout that are in the repair stage.
func repairStage(r *fleet.Rollout) []string {
	hosts := make([]string, 0, 2)

	for i := range r.Hosts {
		if r.Hosts[i].Stage == fleet.StageRepair {
			hosts = append(hosts, r.Hosts[i].Host)
		}
	}

	return hosts
}

// sameAfterRestarts plays a scenario twice, the second time with a restart
// before every reconcile and every action, and requires the two stores to be
// equal after every step.
func sameAfterRestarts(t *testing.T, play func(t *testing.T, restart bool) *replay) {
	t.Helper()

	plain, restarted := play(t, false), play(t, true)
	require.Len(t, restarted.dumps, len(plain.dumps))

	for i := range plain.dumps {
		require.Equal(t, plain.dumps[i], restarted.dumps[i], "the stores differ after step %d", i)
	}
}

// review5OlderRolloutClosedLast plays finding 1a up to the reconcile that
// opens the rollout of 4.0.
//
// Two packages rollouts side by side. P1 (2.0) has zz out; P2 (3.0), created
// later, is aborted and its only lease finishes cleanly. Then P1 is aborted
// while zz is still out for it. 4.0 appears.
func review5OlderRolloutClosedLast(t *testing.T, restart bool) *replay {
	t.Helper()

	r := newReplay(t, restart)

	for i := range 20 {
		r.addHost(fmt.Sprintf("h%02d", i), 0)
	}

	r.addHost("zz", 0, packagesNeed("2.0"))
	r.pass()
	require.Equal(t, []string{"zz"}, r.leased())

	p1 := r.openRollout(packagesKey("2.0"))
	r.running("zz")

	r.advance(time.Minute)
	r.reports("3.0", "h00")
	r.pass()

	p2 := r.openRollout(packagesKey("3.0"))
	require.True(t, p2.CreatedAt.After(p1.CreatedAt))
	require.Equal(t, []string{"h00", "zz"}, r.leased(), "two open rollouts of the group: each goes its own way")

	// The younger rollout is aborted; its host finishes fine.
	r.running("h00")
	r.pass()
	r.abort(p2.ID)
	r.done("h00")
	require.Equal(t, []string{"zz"}, r.leased())

	// The older rollout is aborted while zz is still out for it.
	r.abort(p1.ID)

	r.advance(time.Minute)
	r.reports("4.0", "h01", "h02")
	r.pass()

	return r
}

// Finding 1a: the lease in flight of an aborted rollout is waited for,
// whichever rollout of the group was created last. Before, the rollout of 4.0
// followed "the latest closed rollout of the group, the one created last":
// P2, which had left nothing. zz's lease was in nobody's list, and a canary
// went down next to it.
func TestReview5Finding1aLeaseInFlightOfTheOlderRollout(t *testing.T) {
	r := review5OlderRolloutClosedLast(t, false)
	p3 := r.openRollout(packagesKey("4.0"))

	assert.Equal(t, []string{"zz"}, r.leased(), "no canary next to the lease of the aborted rollout")
	assert.Empty(t, p3.Canary)
	assert.Equal(t, map[string]string{"h01": "after zz", "h02": "after zz"}, queueReasons(r.svc.Plan()))

	for _, q := range r.svc.Plan().Queue {
		assert.Equal(t, WaitOrder, q.Wait)
		assert.Equal(t, "zz", q.After)
	}

	// The lease ends well: the rollout goes on with a canary of its own.
	r.done("zz")
	assert.Len(t, r.leased(), 1)
	assert.NotEmpty(t, r.rollout(p3.ID).Canary)
	assert.Empty(t, repairStage(r.rollout(p3.ID)))

	sameAfterRestarts(t, review5OlderRolloutClosedLast)
}

// Finding 1, the retry: a host that failed in an aborted rollout goes first
// in the rollout that a retry opens for it, whatever was rolled in between.
// Before, the retry followed the latest closed rollout of the group: another
// packages rollout that had run to its end since and left nothing.
func TestReview5Finding1RetryAfterAnotherRolloutOfTheGroupCompleted(t *testing.T) {
	r := newReplay(t, false)
	r.addHost("a", 0, packagesNeed("2.0"))
	r.addHost("b", 0, packagesNeed("2.0"))
	r.addHost("c", 0)
	r.pass()
	require.Equal(t, []string{"a"}, r.leased())

	broke := r.assignment("a").Lease.ID
	r.fail("a")

	p1 := r.openRollout(packagesKey("2.0"))
	r.abort(p1.ID)

	// Another host has another set of packages; its rollout runs to its end
	// (and is closed as superseded: the hosts of 2.0 still report theirs).
	r.advance(time.Minute)
	r.reports("3.0", "c")
	r.pass()

	p2 := r.openRollout(packagesKey("3.0"))
	require.Equal(t, []string{"c"}, r.leased())
	r.done("c")
	require.True(t, r.rollout(p2.ID).State.Terminal())
	require.Equal(t, fleet.HostDone, r.rollout(p2.ID).Host("c").State)

	id := r.retry(p1.ID)
	r.advance(time.Minute)
	r.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
	r.pass()

	reopened := r.rollout(id)
	assert.Equal(t, []string{"a"}, repairStage(reopened))
	assert.Equal(t, brokenBy(broke, p1.ID), reopened.Host("a").Error)
	assert.Equal(t, []string{"a"}, r.leased(), "the host this change broke goes first")
	assert.Equal(t, "after a", queueReasons(r.svc.Plan())["b"])
}

// review5RetriedNextToAnOpenRollout plays findings 1b and 1c up to the
// reconcile that opens the rollout of digest C.
//
// RA (digest A): a fails, the rollout is aborted. RB (digest B): a goes
// first and is repaired; then b is granted. With failB, b fails and RB
// halts. A person retries the old aborted RA: RA' is created after RB and is
// superseded at once (the tag is at B). RB is then aborted (failB) or runs
// to completion. The tag moves to C.
func review5RetriedNextToAnOpenRollout(t *testing.T, restart, failB bool) (r *replay, rb string) {
	t.Helper()

	r = newReplay(t, restart)
	hosts := []string{"a", "b", "c"}

	for _, name := range hosts {
		r.addHost(name, 0)
	}

	r.seesImage(digestA, hosts...)
	r.pass()
	require.Equal(t, []string{"a"}, r.leased())
	r.fail("a")

	ra := r.openRollout(imageKey(imgLighthouse, digestA)).ID
	r.abort(ra)

	r.advance(time.Minute)
	r.seesImage(digestB, hosts...)
	r.pass()

	rb = r.openRollout(imageKey(imgLighthouse, digestB)).ID
	require.Equal(t, []string{"a"}, repairStage(r.rollout(rb)))
	require.Equal(t, []string{"a"}, r.leased())
	r.done("a")
	require.Equal(t, fleet.HostDone, r.rollout(rb).Host("a").State)
	require.Equal(t, []string{"b"}, r.leased())

	if failB {
		r.fail("b")
		require.Equal(t, fleet.RolloutHalted, r.rollout(rb).State)
	}

	// The old aborted rollout is retried next to the open one.
	again := r.retry(ra)
	r.advance(time.Minute)
	r.pass()
	require.Equal(t, fleet.RolloutSuperseded, r.rollout(again).State)
	require.True(t, r.rollout(again).CreatedAt.After(r.rollout(rb).CreatedAt))

	if failB {
		r.abort(rb)
	} else {
		r.done("b")
		r.done("c")
		require.Equal(t, fleet.RolloutCompleted, r.rollout(rb).State)
	}

	r.advance(time.Minute)

	for _, name := range hosts {
		r.sync(name, func(st *protocol.SyncRequest) {
			st.Containers = []protocol.Container{container("c", imgLighthouse, digestB, digestC, r.now)}
			st.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digestC)}
			st.Operation = nil
		})
	}

	r.pass()

	return r, rb
}

// Finding 1b: the host that goes first for digest C is the one that is
// broken, not the one that was repaired. Before, the rollout of C followed
// the latest closed rollout of the image, created last: RA', which had
// inherited a from the aborted RA. So a, repaired in RB, was carried once
// more, and b, which had failed in RB, was an ordinary member.
func TestReview5Finding1bTheBrokenHostGoesFirstNotTheRepairedOne(t *testing.T) {
	r, rb := review5RetriedNextToAnOpenRollout(t, false, true)
	rc := r.openRollout(imageKey(imgLighthouse, digestC))

	assert.Equal(t, []string{"b"}, repairStage(rc), "b failed in RB; a was repaired there")
	assert.Equal(t, brokenBy(r.endedLease("b").ID, rb), rc.Host("b").Error)
	assert.Empty(t, rc.Host("a").Error)
	assert.Empty(t, rc.Canary)
	assert.Equal(t, []string{"b"}, r.leased())
	assert.Equal(t, map[string]string{"a": "after b", "c": "after b"}, queueReasons(r.svc.Plan()))

	sameAfterRestarts(t, func(t *testing.T, restart bool) *replay {
		t.Helper()

		r, _ := review5RetriedNextToAnOpenRollout(t, restart, true)

		return r
	})
}

// Finding 1c: the same with RB running to completion. Nobody is broken, and
// the rollout of C starts with a canary. Before, a was carried from RA' all
// the same.
func TestReview5Finding1cARepairedHostIsNotFirstAgain(t *testing.T) {
	r, _ := review5RetriedNextToAnOpenRollout(t, false, false)
	rc := r.openRollout(imageKey(imgLighthouse, digestC))

	assert.Empty(t, repairStage(rc), "RB completed: every host is in order")
	assert.Equal(t, "a", rc.Canary)
	assert.Equal(t, fleet.StageCanary, rc.Host("a").Stage)
	assert.Equal(t, []string{"a"}, r.leased())
	assert.Equal(t, map[string]string{"b": "after canary a", "c": "after canary a"}, queueReasons(r.svc.Plan()))

	sameAfterRestarts(t, func(t *testing.T, restart bool) *replay {
		t.Helper()

		r, _ := review5RetriedNextToAnOpenRollout(t, restart, false)

		return r
	})
}

// review5Flapping: a lease of a superseded rollout is taken up again, ends
// without having stopped anything, and is reported running once more, four
// times over. Nothing accumulates anywhere: while it is open the successor
// waits for it, and when it is not, it does not.
func review5Flapping(t *testing.T, restart bool) *replay {
	t.Helper()

	r := newReplay(t, restart)
	hosts := []string{"a", "b", "c"}

	for _, name := range hosts {
		r.addHost(name, 0)
	}

	r.seesImage(digestA, hosts...)
	r.pass()
	require.Equal(t, []string{"a"}, r.leased())

	lease := r.assignment("a").Lease
	ra := r.openRollout(imageKey(imgLighthouse, digestA)).ID

	// Nothing from the agent until after the grace period.
	_, err := r.svc.Pause(r.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)

	settings, err := r.svc.Settings(r.ctx)
	require.NoError(t, err)

	for left := settings.LeaseTTL.Std() + leaseStartGrace + time.Second; left > 0; left -= 30 * time.Second {
		r.advance(min(left, 30*time.Second))
		r.pass()
	}

	require.Empty(t, r.leased(), "the lease expired")

	r.advance(time.Minute)
	r.seesImage(digestB, "b", "c")
	r.pass()
	require.Equal(t, fleet.RolloutSuperseded, r.rollout(ra).State)

	_, err = r.svc.Resume(r.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)

	rb := r.openRollout(imageKey(imgLighthouse, digestB)).ID

	report := func(phase protocol.Phase, disrupted bool, status protocol.StepStatus) {
		r.sync("a", func(st *protocol.SyncRequest) {
			st.Operation = &protocol.Operation{
				LeaseID: lease.ID, Phase: phase, Disrupted: disrupted,
				Steps: []protocol.Step{{NeedID: lease.Needs[0].ID, Status: status}},
			}
		})
	}

	for round := range 4 {
		r.advance(5 * time.Second)
		report(protocol.PhaseApplying, true, protocol.StepRunning)
		r.pass()

		open := r.leased()
		require.Contains(t, open, "a", "round %d: taken up again", round)

		if round == 0 {
			require.Equal(t, []string{"a"}, open, "nobody next to the lease of the superseded rollout")
			require.Empty(t, r.rollout(rb).Canary)
			require.Equal(t, "after a", queueReasons(r.svc.Plan())["b"])
		}

		// It ends having stopped nothing after all: expired again, and nobody
		// is broken by that.
		r.advance(5 * time.Second)
		report(protocol.PhaseDone, false, protocol.StepSuperseded)
		r.pass()
		require.NotContains(t, r.leased(), "a", "round %d: expired again", round)
		require.Empty(t, repairStage(r.rollout(rb)), "round %d", round)
	}

	return r
}

func TestReview5LeaseOfASupersededRolloutTakenUpAgainAndAgain(t *testing.T) {
	r := review5Flapping(t, false)
	rb := r.openRollout(imageKey(imgLighthouse, digestB))

	// The successor went on as soon as the lease was not open: one canary.
	assert.NotEmpty(t, rb.Canary)
	assert.Equal(t, []string{rb.Canary}, r.leased())

	sameAfterRestarts(t, review5Flapping)
}

// legacyRow reads a rollout as the store reads a row, from the JSON of an
// earlier build.
func legacyRow(t *testing.T, raw string) *fleet.Rollout {
	t.Helper()

	r := &fleet.Rollout{}
	require.NoError(t, json.Unmarshal([]byte(raw), r), "a row of an earlier build must be readable")

	return r
}

// Finding 2: rows that the commit before the last wrote (a bequest on the
// rollout, no marker that it was recorded) lost what they said: the build
// read them as rows of a build that had recorded nothing, and off members
// that this commit had already settled. Now no build's lists are read at
// all. What every build wrote alike is the lease table, and it says who is
// broken.
func TestReview5Finding2RowsOfEarlierCommits(t *testing.T) {
	// old is the superseded rollout "old" of 2.0 with what the builds kept
	// on it, and members as they stood.
	const (
		inherited  = `"inherited":[{"host":"a","from":"older","lease":"l-gone"}],`
		bequest    = `"bequest":[{"host":"a","from":"older"}],`
		bequeathed = `"bequeathed":true,`
	)

	old := func(t *testing.T, extra, members string) *fleet.Rollout {
		t.Helper()

		return legacyRow(t, `{"id":"old","kind":"packages","key":"`+packagesKey("2.0")+`",`+
			`"title":"packages: docker-ce 2.0","state":"superseded","reason":"newer package versions appeared",`+
			`"paused":false,"approved":false,"force":false,"ignoreBudget":false,`+extra+
			`"createdBy":"scheduler","createdAt":"2026-01-01T08:00:00Z","updatedAt":"2026-01-01T09:00:00Z",`+
			`"endedAt":"2026-01-01T09:00:00Z","hosts":[`+members+`]}`)
	}

	// lease is a's lease for 2.0, which ended after the rollout was closed.
	lease := func(h *harness, id, rollout string, state fleet.LeaseState) *fleet.Lease {
		need := packagesNeed("2.0")
		ended := h.now.Add(-30 * time.Minute)

		return &fleet.Lease{
			ID: id, Host: "a", State: state, GrantedBy: actorScheduler,
			Needs: []protocol.Need{need}, Rollouts: map[string]string{need.ID: rollout},
			GrantedAt: ended.Add(-10 * time.Minute), ExpiresAt: ended.Add(20 * time.Minute), EndedAt: &ended,
			Operation: &protocol.Operation{
				LeaseID: id, Phase: protocol.PhaseFailed, Disrupted: true,
				Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepFailed}},
			},
		}
	}

	for name, tt := range map[string]struct {
		extra, members string
		lease          func(h *harness) *fleet.Lease
		broken         bool
	}{
		"a member whose lease failed after the supersede, bequest without the marker": {
			extra: bequest,
			members: `{"host":"a","state":"pending","stage":3,"updatedAt":"2026-01-01T09:30:00Z"},` +
				`{"host":"b","state":"done","stage":2,"leaseId":"l0","updatedAt":"2026-01-01T08:30:00Z"}`,
			lease:  func(h *harness) *fleet.Lease { return lease(h, "l-a", "old", fleet.LeaseFailed) },
			broken: true,
		},
		"a host that was inherited and never a member, whose lease failed after the supersede": {
			extra:   inherited + bequest,
			members: `{"host":"b","state":"done","stage":2,"leaseId":"l0","updatedAt":"2026-01-01T08:30:00Z"}`,
			lease:   func(h *harness) *fleet.Lease { return lease(h, "l-gone", "older", fleet.LeaseFailed) },
			broken:  true,
		},
		"the same rows of the last commit, with the marker": {
			extra:   inherited + bequest + bequeathed,
			members: `{"host":"b","state":"done","stage":2,"leaseId":"l0","updatedAt":"2026-01-01T08:30:00Z"}`,
			lease:   func(h *harness) *fleet.Lease { return lease(h, "l-gone", "older", fleet.LeaseFailed) },
			broken:  true,
		},
		"a bequest that names a host whose lease was released: the lists say nothing": {
			extra: bequest + bequeathed,
			members: `{"host":"a","state":"pending","stage":1,"carriedFrom":"older",` +
				`"error":"failed in rollout older","updatedAt":"2026-01-01T09:30:00Z"}`,
			lease: func(h *harness) *fleet.Lease {
				l := lease(h, "l-a", "old", fleet.LeaseReleased)
				l.Operation.Phase = protocol.PhaseDone
				l.Operation.Steps[0].Status = protocol.StepDone

				return l
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.store.put(old(t, tt.extra, tt.members))

			l := tt.lease(h)
			require.NoError(t, h.store.Commit(h.ctx, &store.Changes{Leases: []*fleet.Lease{l}}))

			for _, name := range []string{"a", "b", "c"} {
				h.addHost(name, 0, packagesNeed("3.0"))
			}

			h.tick()

			next := h.openRollout(packagesKey("3.0"))

			if !tt.broken {
				assert.Empty(t, repairStage(next))
				assert.Equal(t, "a", next.Canary)
				assert.Empty(t, next.Host("a").Error)

				return
			}

			assert.Equal(t, []string{"a"}, repairStage(next))
			assert.Equal(t, brokenBy(l.ID, l.Rollouts[l.Needs[0].ID]), next.Host("a").Error)
			assert.Equal(t, []string{"a"}, h.leased(), "the host whose last lease for the packages failed goes first")
			assert.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(h.svc.Plan()))
		})
	}

	// A note that an earlier build left on a waiting member is replaced by
	// what the lease table says, or dropped.
	t.Run("the note of an earlier build on a waiting member", func(t *testing.T) {
		h := newHarness(t)
		h.store.put(legacyRow(t, `{"id":"open","kind":"packages","key":"`+packagesKey("3.0")+`",`+
			`"title":"packages: docker-ce 3.0","state":"active","paused":false,"approved":false,"force":false,`+
			`"ignoreBudget":false,"inherited":[{"host":"a","from":"older"}],`+
			`"createdBy":"scheduler","createdAt":"2026-01-01T08:00:00Z","updatedAt":"2026-01-01T09:00:00Z",`+
			`"hosts":[{"host":"a","state":"pending","stage":1,"carriedFrom":"older",`+
			`"error":"failed in rollout older","updatedAt":"2026-01-01T09:30:00Z"}]}`))

		for _, name := range []string{"a", "b"} {
			h.addHost(name, 0, packagesNeed("3.0"))
		}

		_, err := h.svc.Pause(h.ctx, "alice", ScopeRequest{})
		require.NoError(t, err)
		h.tick()

		open := h.rollout("open")
		assert.Empty(t, open.Host("a").Error, "no lease says that a is broken")
		assert.NotEqual(t, fleet.StageRepair, open.Host("a").Stage)
	})
}

// Finding 3: a chain timing that is out of bounds is said so. The beacon
// service validates what it read from the node and publishes why the current
// epoch cannot be placed (NetworkHealth.TimingError); the gate is closed with
// that reason. Before, the gate read "beacon node unreachable" for a node
// that answered, and a single phase out of bounds, scheduled for the far
// future, closed it now.
func TestReview5Finding3ChainTimingOutOfBounds(t *testing.T) {
	build := func(t *testing.T, mod func(n *fleet.NetworkHealth)) *worldBuilder {
		t.Helper()

		b := newWorldBuilder(t)
		b.healthMod = mod
		b.refreshHealth()

		for _, name := range []string{"a", "b", "c"} {
			b.host(name, 32)
			b.wants(name, packagesNeed("2.0"))
		}

		return b
	}

	twoHours := func(n *fleet.NetworkHealth, at uint64) {
		start, ok := n.EpochStart(at)
		if !ok {
			panic("the epoch of the bogus phase cannot be placed")
		}

		n.Phases = append(slices.Clone(n.Phases), fleet.SlotPhase{Epoch: at, Start: start, SlotMs: 2 * fleet.MaxSlotMs})
	}

	t.Run("the current epoch cannot be placed", func(t *testing.T) {
		const reason = "phase 0 (from epoch 0) has slots of 3600000 ms and epochs of 32 slots"

		b := build(t, func(n *fleet.NetworkHealth) {
			n.Phases = fleet.SinglePhase(testGenesis, time.Hour)
			n.TimingError = reason
		})

		// "a" is established as offline: no repair passes a gate that is
		// closed for want of a usable timing.
		b.down("a")

		out := b.step()
		assert.False(t, out.Plan.Gate.OK)
		assert.Equal(t, "chain timing out of bounds: "+reason, out.Plan.Gate.Reasons[0])
		assert.NotContains(t, out.Plan.Gate.Reasons, "beacon node unreachable")
		assert.Equal(t, reason, out.Plan.Gate.Network.TimingError)
		assert.Empty(t, b.leasedHosts())

		for _, q := range out.Plan.Queue {
			assert.Equal(t, "network gate: chain timing out of bounds: "+reason, q.Reason, q.Host)
			assert.Equal(t, WaitGate, q.Wait)
		}
	})

	// A view whose timing places nothing and that says nothing about it (no
	// beacon service behind it) fails safe as before.
	t.Run("no timing and no reason", func(t *testing.T) {
		b := build(t, func(n *fleet.NetworkHealth) { n.Phases = nil })

		out := b.step()
		assert.Equal(t, []string{"beacon node unreachable"}, out.Plan.Gate.Reasons)
		assert.Empty(t, b.leasedHosts())
	})

	t.Run("a phase out of bounds in the future closes nothing", func(t *testing.T) {
		b := build(t, func(n *fleet.NetworkHealth) { twoHours(n, n.CurrentEpoch+1_000_000) })
		require.Error(t, fleet.ValidateTiming(b.w.Health.SlotsPerEpoch, b.w.Health.Phases))

		out := b.step()
		require.True(t, out.Plan.Gate.OK, "gate: %v", out.Plan.Gate.Reasons)
		require.Len(t, b.leasedHosts(), 1)

		// Readiness places its epochs as ever: the canary is released.
		canary := b.leasedHosts()[0]
		b.serve(canary)
		assert.Equal(t, fleet.HostDone, b.mustRollout(packagesKey("2.0")).Host(canary).State)
	})

	// Once the epochs that a host's readiness needs reach into the phase,
	// they cannot be placed: the host is not found ready, and says why.
	t.Run("readiness that reaches into the phase", func(t *testing.T) {
		// The phase begins with the epoch after the one the test starts in.
		at := uint64(0)

		b := build(t, func(n *fleet.NetworkHealth) {
			if at == 0 {
				at = n.CurrentEpoch + 1
			}

			twoHours(n, at)
		})

		out := b.step()
		require.True(t, out.Plan.Gate.OK, "gate: %v", out.Plan.Gate.Reasons)
		require.Len(t, b.leasedHosts(), 1)

		canary := b.leasedHosts()[0]
		b.finish(canary)
		b.step()
		b.advance(5 * time.Second)
		b.step()

		l := b.lease(canary)
		require.NotNil(t, l)
		assert.Equal(t, fleet.LeaseVerifying, l.State)
		assert.Equal(t, noteEpochTiming, l.Readiness)
	})
}

// Finding 4: a lease in flight that does not end can be ended by a person.
// Before, nothing lifted the wait behind the lease of an aborted rollout: a
// skip of its host in the rollout that waited was accepted and changed
// nothing, a hold changed nothing, and forgetting the host was refused.
func TestReview5Finding4APersonEndsALeaseInFlight(t *testing.T) {
	// inFlight: a is out for the rollout of digest A, which a person aborts;
	// b and c see digest B and wait for a's lease.
	inFlight := func(t *testing.T) (r *replay, ra, rb string) {
		t.Helper()

		r = newReplay(t, false)
		hosts := []string{"a", "b", "c"}

		for _, name := range hosts {
			r.addHost(name, 0)
		}

		r.seesImage(digestA, hosts...)
		r.pass()
		require.Equal(t, []string{"a"}, r.leased())
		r.running("a")
		r.pass()

		ra = r.openRollout(imageKey(imgLighthouse, digestA)).ID
		r.abort(ra)

		r.advance(time.Minute)
		r.seesImage(digestB, "b", "c")
		r.pass()

		rb = r.openRollout(imageKey(imgLighthouse, digestB)).ID
		require.Equal(t, "after a", queueReasons(r.svc.Plan())["b"])

		// The agent keeps reporting progress for an hour.
		for range 60 {
			r.advance(time.Minute)
			r.pass()
		}

		require.Equal(t, []string{"a"}, r.leased())
		require.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(r.svc.Plan()))

		return r, ra, rb
	}

	t.Run("skip and hold do not end a lease", func(t *testing.T) {
		r, _, rb := inFlight(t)

		r.skip(rb, "a")
		r.pass()
		assert.Equal(t, "after a", queueReasons(r.svc.Plan())["b"], "a skip is about one rollout, not about a lease")

		_, err := r.svc.Forget(r.ctx, "alice", HoldRequest{Hosts: []string{"a"}})
		require.ErrorIs(t, err, ErrInvalidAction, "a host that holds a lease is not forgotten")

		_, err = r.svc.Hold(r.ctx, "alice", HoldRequest{Hosts: []string{"a"}, Reason: "broken"})
		require.NoError(t, err)
		r.pass()
		assert.Equal(t, []string{"a"}, r.leased(), "a hold does not end a lease either")
		assert.Equal(t, "after a", queueReasons(r.svc.Plan())["b"])
	})

	// Ending the lease is a request (the sixth review, findings6_test.go): the
	// wait lasts until the agent has stopped, and the host is held.
	t.Run("ending the lease lifts the wait once the agent has stopped", func(t *testing.T) {
		r, _, rb := inFlight(t)
		lease := r.assignment("a").Lease.ID

		res, err := r.endLease("a")
		require.NoError(t, err)
		assert.Equal(t, []string{"a"}, res.Hosts)

		// The lease is open and in flight as before: the host is still down.
		r.pass()
		assert.Equal(t, []string{"a"}, r.leased())
		assert.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(r.svc.Plan()))
		assert.True(t, r.assignment("a").Lease.Cancel)

		// The agent stops and says so.
		r.sync("a", func(st *protocol.SyncRequest) {
			st.Operation = &protocol.Operation{
				LeaseID: lease, Phase: protocol.PhaseDone, Disrupted: true,
				Steps: []protocol.Step{{
					NeedID: imageNeed("c", imgLighthouse, digestA).ID, Status: protocol.StepCancelled,
				}},
			}
			st.Hold.Controller = true
			st.Needs = nil
		})
		r.pass()

		ended, err := r.store.GetLease(r.ctx, lease)
		require.NoError(t, err)
		assert.Equal(t, fleet.LeaseFailed, ended.State)
		assert.Equal(t, "ended by operator", ended.Error)
		assert.Nil(t, r.assignment("a").Lease)
		assert.Equal(t, actorScheduler, r.store.lastAuditOf("lease.failed").Actor)

		// Nobody waits for a lease any more, nor for the host: it is held, and
		// the cancel did not break it.
		next := r.rollout(rb)
		assert.Empty(t, repairStage(next))
		assert.Equal(t, []string{"b"}, r.leased())
		assert.Equal(t, "b", next.Canary)

		// The agent still repeats its report. The lease stays ended.
		r.advance(time.Minute)
		r.pass()

		ended, err = r.store.GetLease(r.ctx, lease)
		require.NoError(t, err)
		assert.Equal(t, fleet.LeaseFailed, ended.State)
		assert.Equal(t, []string{"b"}, r.leased())

		// Unheld, a is an ordinary member of the rollout of B.
		_, err = r.svc.Unhold(r.ctx, "alice", HoldRequest{Hosts: []string{"a"}})
		require.NoError(t, err)
		r.sync("a", func(st *protocol.SyncRequest) { st.Hold.Controller = false })
		r.seesImage(digestB, "a")
		r.pass()

		next = r.rollout(rb)
		assert.Empty(t, repairStage(next))
		assert.Empty(t, next.Host("a").Error)
		assert.Equal(t, map[string]string{"a": "after canary b", "c": "after canary b"}, queueReasons(r.svc.Plan()))
	})

	t.Run("what cannot be ended", func(t *testing.T) {
		r, _, _ := inFlight(t)

		_, err := r.svc.EndLease(r.ctx, "alice", EndLeaseRequest{})
		require.ErrorIs(t, err, ErrInvalidAction)

		_, err = r.svc.EndLease(r.ctx, "alice", EndLeaseRequest{Lease: "nosuchlease"})
		require.ErrorIs(t, err, ErrUnknownLease)

		lease := r.assignment("a").Lease.ID
		r.fail("a")

		_, err = r.svc.EndLease(r.ctx, "alice", EndLeaseRequest{Lease: lease})
		require.ErrorIs(t, err, ErrInvalidAction, "a lease that has ended")
		assert.Contains(t, err.Error(), "is failed")
	})

	// The same history with a restart before every step writes the same.
	sameAfterRestarts(t, func(t *testing.T, restart bool) *replay {
		t.Helper()

		r := newReplay(t, restart)

		for _, name := range []string{"a", "b", "c"} {
			r.addHost(name, 0)
		}

		r.seesImage(digestA, "a", "b", "c")
		r.pass()

		lease := r.running("a")

		r.pass()
		r.abort(r.openRollout(imageKey(imgLighthouse, digestA)).ID)
		r.advance(time.Minute)
		r.seesImage(digestB, "b", "c")
		r.pass()

		_, err := r.endLease("a")
		require.NoError(t, err)

		r.pass()
		require.Equal(t, []string{"a"}, r.leased())
		require.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(r.svc.Plan()))

		r.sync("a", func(st *protocol.SyncRequest) {
			st.Operation = &protocol.Operation{
				LeaseID: lease, Phase: protocol.PhaseDone, Disrupted: true,
				Steps: []protocol.Step{{
					NeedID: imageNeed("c", imgLighthouse, digestA).ID, Status: protocol.StepCancelled,
				}},
			}
		})
		r.pass()
		require.Equal(t, []string{"b"}, r.leased(), "the wait is over, and a is held")

		return r
	})
}

// Finding 5: a broken host stays first in every later rollout of the group
// until a lease repairs it. A skip takes it out of one rollout (and of the
// rollouts of the same digest); a hold takes it out of the way for good.
// Before, the docs did not say which of the two lasts.
func TestReview5Finding5SkipIsForOneRolloutHoldIsForGood(t *testing.T) {
	r := newReplay(t, false)
	hosts := []string{"a", "b", "c"}

	for _, name := range hosts {
		r.addHost(name, 0)
	}

	r.seesImage(digestA, hosts...)
	r.pass()
	r.fail("a")
	r.abort(r.openRollout(imageKey(imgLighthouse, digestA)).ID)

	// For digest B it goes first, and fails again.
	r.advance(time.Minute)
	r.seesImage(digestB, hosts...)
	r.pass()

	rb := r.openRollout(imageKey(imgLighthouse, digestB)).ID
	require.Equal(t, []string{"a"}, r.leased())
	r.fail("a")
	require.Equal(t, fleet.RolloutHalted, r.rollout(rb).State)

	// Skipped and the rollout retried, the others go: one canary.
	r.skip(rb, "a")
	r.retry(rb)
	r.pass()
	assert.Equal(t, []string{"b"}, r.leased())
	assert.Equal(t, "b", r.rollout(rb).Canary)

	// The skip was for that rollout. For digest C the host is first again.
	r.done("b")
	require.Equal(t, []string{"c"}, r.leased())
	r.done("c")
	require.Equal(t, fleet.RolloutCompleted, r.rollout(rb).State)
	r.advance(time.Minute)

	for _, name := range hosts {
		r.sync(name, func(st *protocol.SyncRequest) {
			st.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestC, r.now)}
			st.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digestC)}
			st.Operation = nil
		})
	}

	_, err := r.svc.Pause(r.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)
	r.pass()

	rc := r.openRollout(imageKey(imgLighthouse, digestC))
	assert.Equal(t, []string{"a"}, repairStage(rc))
	assert.Empty(t, rc.Canary)

	_, err = r.svc.Resume(r.ctx, "alice", ScopeRequest{})
	require.NoError(t, err)

	// Held, it holds nobody back, in this rollout and in every one after it.
	r.hold("a")
	r.pass()

	rc = r.rollout(rc.ID)
	assert.Equal(t, []string{"b"}, r.leased())
	assert.Equal(t, "b", rc.Canary)
	assert.Equal(t, []string{"a"}, repairStage(rc), "still broken: it goes first once the hold is lifted")
	assert.Equal(t, "after canary b", queueReasons(r.svc.Plan())["c"])
}

// The definition, case by case: what the end of a lease says about its host
// for the thing it carried a step of.
func TestBrokenIsReadOffTheLeases(t *testing.T) {
	image, packages := imageNeed("c", imgLighthouse, digestA), packagesNeed("2.0")
	groupImage, groupPackages := prefixImageKey+imgLighthouse, "packages"
	at := testGenesis

	lease := func(id string, state fleet.LeaseState, minutes int, steps map[string]protocol.StepStatus) *fleet.Lease {
		ended := at.Add(time.Duration(minutes) * time.Minute)
		l := &fleet.Lease{
			ID: id, Host: "a", State: state, Rollouts: map[string]string{},
			GrantedAt: ended.Add(-time.Minute), EndedAt: &ended,
			Operation: &protocol.Operation{LeaseID: id},
		}

		for _, n := range []protocol.Need{image, packages} {
			if status, ok := steps[n.ID]; ok {
				l.Needs = append(l.Needs, n)
				l.Rollouts[n.ID] = "r-" + id
				l.Operation.Steps = append(l.Operation.Steps, protocol.Step{NeedID: n.ID, Status: status})
			}
		}

		return l
	}

	done, failed, superseded := protocol.StepDone, protocol.StepFailed, protocol.StepSuperseded
	only := func(need protocol.Need, status protocol.StepStatus) map[string]protocol.StepStatus {
		return map[string]protocol.StepStatus{need.ID: status}
	}

	for name, tt := range map[string]struct {
		leases []*fleet.Lease
		// image and packages name the lease that broke the host, or "".
		image, packages string
	}{
		"no lease": {},
		"a failed lease breaks the host for what it carried, and for nothing else": {
			leases: []*fleet.Lease{lease("l1", fleet.LeaseFailed, 1, only(image, failed))},
			image:  "l1",
		},
		"a lease that failed without an operation": {
			leases: []*fleet.Lease{{ID: "l1", Host: "a", State: fleet.LeaseFailed, Needs: []protocol.Need{image}}},
			image:  "l1",
		},
		"a later lease that is released with the step done repairs it": {
			leases: []*fleet.Lease{
				lease("l1", fleet.LeaseFailed, 1, only(image, failed)),
				lease("l2", fleet.LeaseReleased, 2, only(image, done)),
			},
		},
		"the order of the table does not matter, the order of the ends does": {
			leases: []*fleet.Lease{
				lease("l2", fleet.LeaseReleased, 2, only(image, done)),
				lease("l1", fleet.LeaseFailed, 1, only(image, failed)),
			},
		},
		"it fails again after the repair": {
			leases: []*fleet.Lease{
				lease("l1", fleet.LeaseFailed, 1, only(image, failed)),
				lease("l2", fleet.LeaseReleased, 2, only(image, done)),
				lease("l3", fleet.LeaseFailed, 3, only(image, done)),
			},
			image: "l3",
		},
		"a lease that expired stopped nothing: it neither breaks nor repairs": {
			leases: []*fleet.Lease{
				lease("l1", fleet.LeaseFailed, 1, only(image, failed)),
				lease("l2", fleet.LeaseExpired, 2, only(image, superseded)),
				lease("l3", fleet.LeaseExpired, 3, nil),
			},
			image: "l1",
		},
		"an expired lease alone": {
			leases: []*fleet.Lease{lease("l1", fleet.LeaseExpired, 1, only(image, superseded))},
		},
		"released with the step not done: no repair, and no failure": {
			leases: []*fleet.Lease{
				lease("l1", fleet.LeaseFailed, 1, only(packages, failed)),
				lease("l2", fleet.LeaseReleased, 2, map[string]protocol.StepStatus{image.ID: done, packages.ID: superseded}),
			},
			packages: "l1",
		},
		"a success for another thing repairs nothing": {
			leases: []*fleet.Lease{
				lease("l1", fleet.LeaseFailed, 1, only(image, failed)),
				lease("l2", fleet.LeaseReleased, 2, only(packages, done)),
			},
			image: "l1",
		},
		"failed on another step with this one done: the host is spared for this": {
			leases: []*fleet.Lease{
				lease("l1", fleet.LeaseFailed, 1, map[string]protocol.StepStatus{image.ID: done, packages.ID: failed}),
			},
			packages: "l1",
		},
		"failed with every step done (not ready, need still reported): all of them": {
			leases: []*fleet.Lease{
				lease("l1", fleet.LeaseFailed, 1, map[string]protocol.StepStatus{image.ID: done, packages.ID: done}),
			},
			image: "l1", packages: "l1",
		},
		"an open lease says nothing yet": {
			leases: []*fleet.Lease{
				lease("l1", fleet.LeaseFailed, 1, only(image, failed)),
				{ID: "l2", Host: "a", State: fleet.LeaseRunning, Needs: []protocol.Need{image}},
			},
			image: "l1",
		},
		"the lease of a manual action belongs to no group": {
			leases: []*fleet.Lease{{
				ID: "l1", Host: "a", State: fleet.LeaseFailed,
				Needs: []protocol.Need{{ID: "m", Kind: protocol.NeedImage, DirectiveID: "d1", Image: image.Image}},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ld := newLedger(tt.leases)

			for group, want := range map[string]string{groupImage: tt.image, groupPackages: tt.packages} {
				got := ""
				if l := ld.brokenSince("a", group); l != nil {
					got = l.ID
				}

				assert.Equal(t, want, got, group)
			}

			assert.Nil(t, ld.brokenSince("b", groupImage), "another host")
			assert.Nil(t, ld.brokenSince("a", ""), "a rollout without a group")

			// The digest of the table that the service hands to a reconcile
			// says the same as the table.
			again := newLedger(ld.leases())

			for _, group := range []string{groupImage, groupPackages} {
				assert.Equal(t, ld.brokenSince("a", group), again.brokenSince("a", group), group)
			}
		})
	}
}
