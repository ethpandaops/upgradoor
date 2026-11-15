package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The tests in this file pin two rules of docs/scheduler.md:
//
//   - Ordering, 4: a rollout is paused when it has its first success, if the
//     setting of its kind asks for it (pauseAfterCanary...);
//   - Readiness, 3: each client of a host that is ready otherwise must have
//     readyPeerShare of the median peer count of the reference hosts.
//
// Each test says in its name which way of breaking the rule it catches.

// --- pause after the canary ---

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

// canaryPauseReplay plays a packages rollout of four hosts with the pause
// after the canary on, up to the end: the canary, the pause, a person's
// resume, the rest.
func canaryPauseReplay(t *testing.T, restart bool) *replay {
	t.Helper()

	r := newReplay(t, restart)
	r.set(func(s *fleet.Settings) {
		s.PauseAfterCanaryPackages = true
		s.MaxUnavailablePercent = 100
	})

	for _, name := range []string{"a", "b", "c", "d"} {
		r.addHost(name, 0, packagesNeed("2.0"))
	}

	r.pass()
	require.Equal(r.t, []string{"a"}, r.leased(), "the canary, alone")

	r.done("a")

	return r
}

// The rollout is paused in the reconcile that releases its canary, with the
// pause a person sets and a reason of its own; it grants nothing until a
// person resumes it, and it is not paused a second time. Catches: no pause;
// pause twice.
func TestCanaryPauseHappensOnceAndResumeContinues(t *testing.T) {
	r := canaryPauseReplay(t, false)
	id := r.openRollout(packagesKey("2.0")).ID

	paused := r.rollout(id)
	assert.True(t, paused.Paused)
	assert.Equal(t, "paused after the canary; resume to continue", paused.PauseReason)
	assert.Equal(t, fleet.RolloutActive, paused.State, "the pause is no state of its own")
	assert.Equal(t, "a", paused.Canary)
	assert.Equal(t, fleet.HostDone, paused.Host("a").State)
	assert.Empty(t, r.leased())

	entry := r.store.lastAuditOf("rollout.paused")
	assert.Equal(t, "scheduler", entry.Actor)
	assert.Equal(t, id, entry.Target)
	assert.Equal(t, "paused after the canary; resume to continue", entry.Detail["reason"])
	assert.NotEmpty(t, entry.Detail["lease"], "the lease that was the first success")

	plan := r.svc.Plan()
	assert.Equal(t, map[string]string{
		"b": "paused after the canary; resume to continue",
		"c": "paused after the canary; resume to continue",
		"d": "paused after the canary; resume to continue",
	}, queueReasons(plan))

	for _, q := range plan.Queue {
		assert.Equal(t, WaitRolloutPaused, q.Wait, "the wait kind of every paused rollout")
		assert.Equal(t, id, q.Rollout)
	}

	// It waits for a person, however long.
	for range 5 {
		r.advance(time.Minute)
		r.pass()
	}

	assert.Empty(t, r.leased())
	assert.Equal(t, 1, r.audited("rollout.paused"))

	// The existing action lifts it, and takes the reason with it.
	res := r.resume(id)
	assert.Equal(t, "rollout resumed", res.Message)
	assert.False(t, r.rollout(id).Paused)
	assert.Empty(t, r.rollout(id).PauseReason)

	r.pass()
	assert.Equal(t, []string{"b", "c", "d"}, r.leased(), "the rest, as the budget allows")

	// The successes that follow are not the first: no second pause.
	for _, name := range []string{"b", "c"} {
		r.running(name)
	}

	r.done("d")
	assert.False(t, r.rollout(id).Paused, "resuming does not pause again")

	r.done("b")
	r.done("c")

	assert.Equal(t, fleet.RolloutCompleted, r.rollout(id).State)
	assert.Equal(t, 1, r.audited("rollout.paused"), "once per rollout")
}

// A restart at any point gives the same rows: the pause is written in the
// commit that releases the canary, so there is no moment between the two.
func TestCanaryPauseSurvivesRestarts(t *testing.T) {
	sameAfterRestarts(t, func(t *testing.T, restart bool) *replay {
		t.Helper()

		r := canaryPauseReplay(t, restart)
		id := r.openRollout(packagesKey("2.0")).ID

		r.advance(time.Minute)
		r.pass()
		r.resume(id)
		r.pass()
		r.done("b")

		return r
	})
}

// The release of the canary and the pause are one outcome, and the rows read
// back from the store (through JSON, as the store keeps them) give the same
// answer in the next reconcile: nothing more is written, nobody is granted.
func TestCanaryPauseIsWrittenWithTheReleaseAndRoundTrips(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PauseAfterCanaryPackages = true
	b.w.Settings.MaxUnavailablePercent = 100

	for _, name := range []string{"a", "b", "c"} {
		b.host(name, 0)
		b.wants(name, packagesNeed("2.0"))
	}

	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())
	b.finish("a")
	b.step()
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)

	out := b.step()
	assert.Subset(t, auditActions(out), []string{"lease.released", "rollout.paused"}, "one outcome, one commit")
	require.Len(t, out.Rollouts, 1)
	assert.True(t, out.Rollouts[0].Paused)

	raw, err := json.Marshal(out.Rollouts[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"pauseReason":"paused after the canary; resume to continue"`)

	// The store's round trip.
	stored := *b.w
	stored.Rollouts = []*fleet.Rollout{storedRollout(b.mustRollout(packagesKey("2.0")))}
	stored.Ended = make([]*fleet.Lease, 0, len(b.w.Ended))
	stored.ClosedLeases = make(map[string]*fleet.Lease, len(b.w.Ended))

	for _, l := range b.w.Ended {
		stored.Ended = append(stored.Ended, storedLease(l))
		stored.ClosedLeases[l.ID] = storedLease(l)
	}

	again := Reconcile(&stored)
	assert.Empty(t, again.Rollouts)
	assert.Empty(t, again.Leases)
	assert.Empty(t, again.Audit)
	assert.Equal(t, map[string]string{
		"b": "paused after the canary; resume to continue",
		"c": "paused after the canary; resume to continue",
	}, queueReasons(again.Plan))
}

// A rollout with a single member has no canary and nobody to wait: it
// completes. So does a rollout whose other members are all done when the
// first success comes. Catches: pause on a single member.
func TestCanaryPauseNotForARolloutThatItsFirstSuccessCloses(t *testing.T) {
	t.Run("a single member", func(t *testing.T) {
		r := newReplay(t, false)
		r.set(func(s *fleet.Settings) { s.PauseAfterCanaryPackages = true })
		r.addHost("a", 0, packagesNeed("2.0"))
		r.addHost("b", 0)
		r.pass()

		id := r.openRollout(packagesKey("2.0")).ID
		r.done("a")

		closed := r.rollout(id)
		assert.Equal(t, fleet.RolloutCompleted, closed.State)
		assert.False(t, closed.Paused)
		assert.Empty(t, closed.PauseReason)
		assert.Zero(t, r.audited("rollout.paused"))
	})

	t.Run("the other member's need went away", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.PauseAfterCanaryPackages = true

		for _, name := range []string{"a", "b"} {
			b.host(name, 0)
			b.wants(name, packagesNeed("2.0"))
		}

		b.step()
		require.Equal(t, []string{"a"}, b.leasedHosts())
		b.finish("a")
		b.step()
		b.advance(5 * time.Second)
		b.step()
		b.advance(2 * testEpoch)

		// Somebody upgraded b by hand: the sync that releases a no longer
		// carries b's need.
		b.get("b").Status.Needs = nil

		out := b.step()
		assert.Contains(t, auditActions(out), "lease.released")
		assert.NotContains(t, auditActions(out), "rollout.paused")

		closed := b.closed(packagesKey("2.0"))
		require.NotNil(t, closed)
		assert.Equal(t, fleet.RolloutCompleted, closed.State)
		assert.False(t, closed.Paused)
	})
}

// A rollout that is superseded is closed as it stands; the rollout of the
// next digest is a new one, with a canary and a pause of its own.
func TestCanaryPauseOfASupersededRolloutIsNotInherited(t *testing.T) {
	r := newReplay(t, false)
	r.set(func(s *fleet.Settings) {
		s.PauseAfterCanaryImage = true
		s.MaxUnavailablePercent = 100
	})

	hosts := []string{"a", "b", "c"}
	for _, name := range hosts {
		r.addHost(name, 0)
	}

	r.seesImage(digestA, hosts...)
	r.pass()
	require.Equal(t, []string{"a"}, r.leased())

	first := r.openRollout(imageKey(imgLighthouse, digestA))
	r.done("a")
	require.True(t, r.rollout(first.ID).Paused)

	// The tag moves while the rollout waits for a person.
	r.advance(time.Minute)
	r.seesImage(digestB, hosts...)
	r.pass()

	assert.Equal(t, fleet.RolloutSuperseded, r.rollout(first.ID).State)

	second := r.openRollout(imageKey(imgLighthouse, digestB))
	assert.False(t, second.Paused, "the pause was the other rollout's")
	assert.Empty(t, second.PauseReason)
	require.Len(t, r.leased(), 1, "a canary of its own")

	canary := r.leased()[0]
	assert.Equal(t, canary, second.Canary)

	r.done(canary)

	second = r.rollout(second.ID)
	assert.True(t, second.Paused, "and a pause of its own")
	assert.Equal(t, "paused after the canary; resume to continue", second.PauseReason)
	assert.Equal(t, 2, r.audited("rollout.paused"))
}

// The pause comes after the rollout's first success, whichever host that is:
// when a broken host goes first and is repaired, its repair is the first
// success (Ordering, 1), so the rollout pauses then and has no canary.
func TestCanaryPauseAfterABrokenHostWasRepaired(t *testing.T) {
	r := newReplay(t, false)
	r.set(func(s *fleet.Settings) {
		s.PauseAfterCanaryPackages = true
		s.MaxUnavailablePercent = 100
	})

	for _, name := range []string{"a", "b", "c"} {
		r.addHost(name, 0, packagesNeed("2.0"))
	}

	r.pass()
	require.Equal(t, []string{"a"}, r.leased())
	r.fail("a")

	id := r.openRollout(packagesKey("2.0")).ID
	require.Equal(t, fleet.RolloutHalted, r.rollout(id).State)
	assert.False(t, r.rollout(id).Paused, "a failure is no success")

	r.retry(id)
	r.advance(time.Minute)
	r.sync("a", func(st *protocol.SyncRequest) { st.Operation = nil })
	r.pass()
	require.Equal(t, []string{"a"}, r.leased(), "the host this change broke goes first")
	require.Equal(t, []string{"a"}, repairStage(r.rollout(id)))

	r.done("a")

	paused := r.rollout(id)
	assert.True(t, paused.Paused)
	assert.Equal(t, "paused after the canary; resume to continue", paused.PauseReason)
	assert.Empty(t, paused.Canary, "the repair was the first success: there is no canary")
	assert.Empty(t, r.leased())

	r.resume(id)
	r.pass()
	assert.Equal(t, []string{"b", "c"}, r.leased(), "after the first success nobody goes alone")
	assert.Equal(t, 1, r.audited("rollout.paused"))
}

// The first success may be a lease this database did not grant: an operation
// that the agent reports and the controller adopts (a lost database). Such a
// lease carries no needs, so the kind is the rollout's, not the lease's.
// Found by the simulation (seed 1165).
func TestCanaryPauseAfterAnAdoptedLease(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PauseAfterCanaryReboot = true
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.Paused = true

	for _, name := range []string{"a", "b", "c"} {
		b.host(name, 0)
		b.wants(name, rebootNeed())
	}

	b.step()
	require.Empty(t, b.leasedHosts())
	require.Equal(t, fleet.HostPending, b.mustRollout(keyReboot).Host("a").State)

	// The agent of a is at work on a lease nobody here knows, and reports no
	// need while it is.
	b.w.Settings.Paused = false

	st := *b.get("a").Status
	st.Needs = nil
	st.Operation = &protocol.Operation{
		LeaseID: "lost", Phase: protocol.PhaseApplying, Disrupted: true,
		Steps: []protocol.Step{{NeedID: protocol.RebootNeedID, Status: protocol.StepRunning}},
	}
	b.get("a").Status = &st

	out := b.step()
	require.Contains(t, auditActions(out), "lease.adopted")
	require.Equal(t, []string{"a"}, b.leasedHosts(), "nobody next to the lease that was adopted")
	require.Empty(t, b.lease("a").Needs)

	b.report("a", protocol.PhaseDone, true)
	b.get("a").Status.Operation.Steps = []protocol.Step{{NeedID: protocol.RebootNeedID, Status: protocol.StepDone}}
	b.step()
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)

	out = b.step()
	require.Nil(t, b.lease("a"))
	assert.Subset(t, auditActions(out), []string{"lease.released", "rollout.paused"})

	r := b.mustRollout(keyReboot)
	assert.True(t, r.Paused)
	assert.Equal(t, "paused after the canary; resume to continue", r.PauseReason)
	assert.Empty(t, b.leasedHosts())
}

// answerReboot lets the host answer its reboot directives with a need each.
func (r *replay) answerReboot(host string) {
	r.t.Helper()

	dirs := r.assignment(host).Directives
	require.NotEmpty(r.t, dirs, "no directive for %s", host)

	r.sync(host, func(st *protocol.SyncRequest) {
		for _, d := range dirs {
			st.DirectiveAcks = append(st.DirectiveAcks, protocol.DirectiveAck{ID: d.ID, Status: protocol.DirectiveAccepted})
			st.Needs = append(st.Needs, protocol.Need{
				ID: protocol.ManualNeedID(d.ID), Kind: protocol.NeedReboot, DirectiveID: d.ID,
				Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
			})
		}
	})
}

// The setting is per kind. A staggered manual rollout follows the setting of
// what it does (a reboot: pauseAfterCanaryReboot); one that is not staggered,
// or forced, has no canary and is never paused.
func TestCanaryPauseFollowsTheKind(t *testing.T) {
	t.Run("another kind's setting pauses nothing", func(t *testing.T) {
		r := newReplay(t, false)
		r.set(func(s *fleet.Settings) {
			s.PauseAfterCanaryImage, s.PauseAfterCanaryReboot = true, true
			s.MaxUnavailablePercent = 100
		})

		for _, name := range []string{"a", "b", "c"} {
			r.addHost(name, 0, packagesNeed("2.0"))
		}

		r.pass()
		r.done("a")

		assert.False(t, r.openRollout(packagesKey("2.0")).Paused)
		assert.Equal(t, []string{"b", "c"}, r.leased())
		assert.Zero(t, r.audited("rollout.paused"))
	})

	manual := func(t *testing.T, req RebootRequest, set func(s *fleet.Settings)) (*replay, string) {
		t.Helper()

		r := newReplay(t, false)
		r.set(func(s *fleet.Settings) {
			s.MaxUnavailablePercent = 100
			set(s)
		})

		for _, name := range []string{"a", "b", "c"} {
			r.addHost(name, 0)
		}

		req.All = true

		res, err := r.svc.Reboot(r.ctx, "alice", req)
		require.NoError(t, err)

		for _, name := range []string{"a", "b", "c"} {
			r.answerReboot(name)
		}

		r.advance(0)
		r.pass()

		return r, res.Rollout
	}

	t.Run("a staggered reboot follows pauseAfterCanaryReboot", func(t *testing.T) {
		r, id := manual(t, RebootRequest{Staggered: true}, func(s *fleet.Settings) { s.PauseAfterCanaryReboot = true })
		require.Equal(t, []string{"a"}, r.leased())

		r.done("a")
		assert.True(t, r.rollout(id).Paused)
		assert.Equal(t, "paused after the canary; resume to continue", r.rollout(id).PauseReason)
		assert.Empty(t, r.leased())

		r.resume(id)
		r.pass()
		assert.Equal(t, []string{"b", "c"}, r.leased())
	})

	t.Run("a staggered reboot does not follow the image setting", func(t *testing.T) {
		r, id := manual(t, RebootRequest{Staggered: true}, func(s *fleet.Settings) {
			s.PauseAfterCanaryImage, s.PauseAfterCanaryPackages = true, true
		})

		r.done("a")
		assert.False(t, r.rollout(id).Paused)
		assert.Equal(t, []string{"b", "c"}, r.leased())
	})

	for name, req := range map[string]RebootRequest{
		"a reboot now has no canary":              {},
		"a forced staggered reboot has no canary": {Staggered: true, Force: true},
	} {
		t.Run(name, func(t *testing.T) {
			r, id := manual(t, req, func(s *fleet.Settings) { s.PauseAfterCanaryReboot = true })
			require.True(t, r.rollout(id).Force)

			// One host per reconcile, readiness not awaited.
			for range 6 {
				for _, host := range r.leased() {
					if lease := r.assignment(host).Lease; lease != nil {
						r.sync(host, func(st *protocol.SyncRequest) {
							st.Operation = &protocol.Operation{
								LeaseID: lease.ID, Phase: protocol.PhaseDone, Disrupted: true,
								Steps: []protocol.Step{{NeedID: lease.Needs[0].ID, Status: protocol.StepDone}},
							}
							st.Needs = nil
						})
					}
				}

				r.advance(5 * time.Second)
				r.pass()
			}

			assert.Equal(t, fleet.RolloutCompleted, r.rollout(id).State)
			assert.False(t, r.rollout(id).Paused)
			assert.Zero(t, r.audited("rollout.paused"))
		})
	}
}

// What a person did stays: a rollout that a person paused before its first
// success keeps that pause (and its reason), and the first success, which
// comes once, is over when it is resumed. And the setting is asked when the
// first success comes: turning it on later pauses nothing.
func TestCanaryPauseLeavesAPersonsPauseAndComesOnlyWithTheFirstSuccess(t *testing.T) {
	t.Run("paused by a person while the canary is out", func(t *testing.T) {
		r := newReplay(t, false)
		r.set(func(s *fleet.Settings) {
			s.PauseAfterCanaryPackages = true
			s.MaxUnavailablePercent = 100
		})

		for _, name := range []string{"a", "b", "c"} {
			r.addHost(name, 0, packagesNeed("2.0"))
		}

		r.pass()

		id := r.openRollout(packagesKey("2.0")).ID
		_, err := r.svc.Pause(r.ctx, "alice", ScopeRequest{Rollout: id})
		require.NoError(t, err)

		r.done("a")
		assert.True(t, r.rollout(id).Paused)
		assert.Empty(t, r.rollout(id).PauseReason, "the person's pause")
		assert.Equal(t, "rollout paused", queueReasons(r.svc.Plan())["b"])
		assert.Zero(t, r.audited("rollout.paused"))

		r.resume(id)
		r.pass()
		assert.Equal(t, []string{"b", "c"}, r.leased())
	})

	t.Run("the setting is turned on after the first success", func(t *testing.T) {
		r := newReplay(t, false)
		r.set(func(s *fleet.Settings) { s.MaxUnavailablePercent = 34 })

		for _, name := range []string{"a", "b", "c"} {
			r.addHost(name, 0, packagesNeed("2.0"))
		}

		r.pass()
		r.done("a")
		require.Equal(t, []string{"b"}, r.leased(), "one of three at a time")

		r.set(func(s *fleet.Settings) { s.PauseAfterCanaryPackages = true })
		r.done("b")

		assert.False(t, r.openRollout(packagesKey("2.0")).Paused)
		assert.Equal(t, []string{"c"}, r.leased())
	})
}

// goldenOff is the SHA-256 of the store after every step of an existing
// pinned scenario (findings5_test.go, review5OlderRolloutClosedLast, played on
// to the canary of the rollout it opens and to the hosts after it), as the
// build before the two settings wrote it (commit b6c4a48).
const goldenOff = "420aaeaf2097384f66d2011c0e32fe7dab9be98e15dae50b51b86e355d442ae6"

// offScenario plays that scenario to its end.
func offScenario(t *testing.T, tune func(r *replay)) string {
	t.Helper()

	r := review5OlderRolloutClosedLast(t, false)
	if tune != nil {
		tune(r)
	}

	r.done("zz")
	require.Len(t, r.leased(), 1)

	canary := r.leased()[0]
	r.done(canary)

	for len(r.leased()) > 0 {
		r.done(r.leased()[0])
	}

	sum := sha256.Sum256([]byte(strings.Join(r.dumps, "\n--\n")))

	return hex.EncodeToString(sum[:])
}

// With both settings off the scheduler writes, byte for byte, what the build
// before them wrote: no pause, no reason, no comparison on any row.
func TestCanaryPauseAndPeerShareOffAreTheBuildBefore(t *testing.T) {
	assert.Equal(t, goldenOff, offScenario(t, nil), "the defaults: no pause after the canary")

	assert.Equal(t, goldenOff, offScenario(t, func(r *replay) {
		r.set(func(s *fleet.Settings) {
			s.PauseAfterCanaryImage, s.PauseAfterCanaryPackages, s.PauseAfterCanaryReboot = false, false, false
			s.ReadyPeerShare = 0
		})
	}), "both switched off explicitly")

	on := offScenario(t, func(r *replay) {
		r.set(func(s *fleet.Settings) { s.PauseAfterCanaryPackages = true })
	})
	assert.NotEqual(t, goldenOff, on, "the scenario is one in which the setting matters")
}

// A rollout that is halted says so, also when it is paused after its canary:
// the failure is what a person looks at first. Retry brings the pause to the
// front, and until somebody resumes nobody goes, a host that is still broken
// included. Catches: the pause hiding the halt.
func TestCanaryPauseOfAHaltedRolloutShowsTheHaltFirst(t *testing.T) {
	const pauseReason = "paused after the canary; resume to continue"

	b := newWorldBuilder(t)
	b.w.Settings.PauseAfterCanaryPackages = true
	b.w.Settings.MaxUnavailablePercent = 100

	for _, name := range []string{"a", "b", "c", "d"} {
		b.host(name, 0)
		b.wants(name, packagesNeed("2.0"))
	}

	// a and b failed their last packages lease: both go first, together.
	b.broke("a", packagesNeed("1.5"))
	b.broke("b", packagesNeed("1.5"))

	b.step()
	require.Equal(t, []string{"a", "b"}, b.leasedHosts())

	// a fails, b is repaired: the rollout halts, and has its first success.
	b.report("a", protocol.PhaseFailed, true)
	b.finish("b")
	b.step()
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)

	out := b.step()
	require.Nil(t, b.lease("b"))
	require.Contains(t, auditActions(out), "rollout.paused")

	key := packagesKey("2.0")
	r := b.mustRollout(key)
	require.Equal(t, fleet.RolloutHalted, r.State)
	require.True(t, r.Paused)
	require.Equal(t, pauseReason, r.PauseReason)

	halted := "rollout halted: a: container did not start"
	assert.Equal(t, map[string]string{"c": halted, "d": halted}, queueReasons(out.Plan))

	for _, q := range out.Plan.Queue {
		assert.Equal(t, WaitHalted, q.Wait)
		assert.Equal(t, r.ID, q.Rollout)
	}

	// A person retries: what the action does to the rollout.
	retried := cloneRollout(r)
	retried.State, retried.Reason = fleet.RolloutActive, ""
	*retried.Host("a") = fleet.RolloutHost{Host: "a", State: fleet.HostPending, UpdatedAt: b.w.Now}
	b.w.Rollouts = []*fleet.Rollout{retried}

	st := *b.get("a").Status
	st.Operation = nil
	b.get("a").Status = &st

	out = b.step()
	assert.Equal(t, map[string]string{"a": pauseReason, "c": pauseReason, "d": pauseReason}, queueReasons(out.Plan),
		"now the pause shows; the host that is still broken waits behind it too")
	assert.Empty(t, b.leasedHosts())

	for _, q := range out.Plan.Queue {
		assert.Equal(t, WaitRolloutPaused, q.Wait)
	}

	// And resumes: what that action does.
	resumed := cloneRollout(b.mustRollout(key))
	resumed.Paused, resumed.PauseReason = false, ""
	b.w.Rollouts = []*fleet.Rollout{resumed}

	b.step()
	assert.Equal(t, []string{"a"}, b.leasedHosts(), "the broken host first")

	// The other order: resumed while halted, the halt still shows.
	t.Run("a person's pause on a halted rollout", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 0)
		b.host("b", 0)
		b.wants("a", packagesNeed("2.0"))
		b.wants("b", packagesNeed("2.0"))
		b.step()
		b.report("a", protocol.PhaseFailed, true)
		b.step()

		paused := cloneRollout(b.mustRollout(packagesKey("2.0")))
		paused.Paused = true
		b.w.Rollouts = []*fleet.Rollout{paused}

		out := b.step()
		assert.Equal(t, map[string]string{"b": "rollout halted: a: container did not start"}, queueReasons(out.Plan))
	})
}

// The kind a rollout serves is its own; a manual rollout serves what its
// directives ask for, and one without any (which no action creates) serves
// nothing and is never paused.
func TestCanaryPauseKindOfARollout(t *testing.T) {
	directive := func(kind protocol.DirectiveKind) *fleet.Directive {
		return &fleet.Directive{
			Directive: protocol.Directive{ID: "d-" + string(kind), Kind: kind},
			Host:      "a", State: fleet.DirectiveAccepted, RolloutID: "m-" + string(kind),
		}
	}

	settings := fleet.DefaultSettings()
	settings.PauseAfterCanaryImage, settings.PauseAfterCanaryPackages, settings.PauseAfterCanaryReboot = true, true, true

	p := newPass(&World{
		Settings:   settings,
		Directives: []*fleet.Directive{directive(protocol.DirectiveRoll), directive(protocol.DirectiveReboot)},
	})

	for _, tt := range []struct {
		r    fleet.Rollout
		want protocol.NeedKind
	}{
		{fleet.Rollout{Kind: fleet.RolloutImage}, protocol.NeedImage},
		{fleet.Rollout{Kind: fleet.RolloutPackages}, protocol.NeedPackages},
		{fleet.Rollout{Kind: fleet.RolloutReboot}, protocol.NeedReboot},
		{fleet.Rollout{Kind: fleet.RolloutManual, ID: "m-roll"}, protocol.NeedImage},
		{fleet.Rollout{Kind: fleet.RolloutManual, ID: "m-reboot"}, protocol.NeedReboot},
		{fleet.Rollout{Kind: fleet.RolloutManual, ID: "m-none"}, ""},
	} {
		assert.Equal(t, tt.want, p.servedKind(&tt.r), "%s %s", tt.r.Kind, tt.r.ID)
		assert.Equal(t, tt.want != "", settings.PauseAfterCanaryFor(p.servedKind(&tt.r)))
	}
}

// --- comparative readiness ---

// comparing builds a fleet in which "a" has just reported its operation
// done: a and one reference host per node of refs ("ref1", ...), all without
// validators. readyEpochs is 0, so a is ready at its first healthy sync and
// the comparison with the fleet is all that can keep its lease; prepare runs
// before the first reconcile and may change that.
func comparing(t *testing.T, share float64, prepare func(b *worldBuilder), refs ...protocol.NodeHealth) *worldBuilder {
	t.Helper()

	b := newWorldBuilder(t)
	b.w.Settings.ReadyPeerShare = share
	b.w.Settings.ReadyEpochs = 0
	b.w.Settings.MinPeers = 1
	b.w.Settings.MaxUnavailablePercent = 100

	b.host("a", 0)
	b.wants("a", packagesNeed("2.0"))

	for i, node := range refs {
		b.host(fmt.Sprintf("ref%d", i+1), 0).Status.Node = node
	}

	if prepare != nil {
		prepare(b)
	}

	b.step()
	require.Contains(t, b.leasedHosts(), "a")
	b.finish("a")
	b.step()
	require.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	return b
}

// seen lets "a" sync with the node, five seconds on, and reconciles. The
// hosts in silent do not sync.
func (b *worldBuilder) seen(node protocol.NodeHealth, silent ...string) *fleet.Lease {
	b.t.Helper()

	b.get("a").Status.Node = node
	b.advance(5*time.Second, silent...)
	b.step()

	return b.lease("a")
}

const (
	noteNodeWaiting = "node at head with peers, waiting for it to stay so for 6m24s"
	noteReadyNode   = "ready: node at head with peers"
)

// A host that is ready otherwise and whose client has fewer peers than the
// share of the fleet's median keeps its lease, with the reason; at the share
// it is released. Catches: ignore the share.
func TestPeerShareBelowWaitsAtOrAboveReleases(t *testing.T) {
	b := comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), nodeWith(31, 31))

	for _, tt := range []struct {
		node protocol.NodeHealth
		note string
	}{
		{nodeWith(31, 4), "CL peers 4, the fleet's median is 31 (needs 16)"},
		{nodeWith(15, 31), "EL peers 15, the fleet's median is 31 (needs 16)"},
		{nodeWith(4, 4), "EL peers 4, the fleet's median is 31 (needs 16)"},
	} {
		l := b.seen(tt.node)
		require.NotNil(t, l)
		assert.Equal(t, fleet.LeaseVerifying, l.State)
		assert.Equal(t, tt.note, l.Readiness)
		assert.NotNil(t, l.SyncedSince, "the run of healthy syncs stays")
	}

	assert.Equal(t, []fleet.PeerCheck{
		{Layer: "EL", Peers: 4, Median: 31, Needs: 16},
		{Layer: "CL", Peers: 4, Median: 31, Needs: 16},
	}, b.lease("a").PeerChecks, "both clients, for whoever looks")

	// Exactly the share is enough.
	b.get("a").Status.Node = nodeWith(16, 16)
	b.advance(5 * time.Second)

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, noteReadyNode, releasedReadiness(t, out))
	assert.Equal(t, []fleet.PeerCheck{
		{Layer: "EL", Peers: 16, Median: 31, Needs: 16},
		{Layer: "CL", Peers: 16, Median: 31, Needs: 16},
	}, b.lastLease("a").PeerChecks, "what it was released on")
	assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)
}

// The comparison is made once, when the host is ready otherwise, with the
// fleet as it is then; it never ends the run of healthy syncs. So references
// that move while the host waits out its epoch cost it nothing, and a host
// that falls short at the end is released in the first reconcile in which it
// does not, without waiting another epoch. Catches: the comparison as part of
// every sync's check; the comparison ending the run.
func TestPeerShareIsAskedOnceAndDoesNotEndTheRun(t *testing.T) {
	b := comparing(t, 0.5, func(b *worldBuilder) { b.w.Settings.ReadyEpochs = 1 },
		nodeWith(31, 31), nodeWith(31, 31), nodeWith(31, 31))

	// 16 of a median of 31: at the share. The run begins; nothing is compared
	// while it is short of an epoch.
	l := b.seen(nodeWith(16, 16))
	require.NotNil(t, l)
	assert.Equal(t, noteNodeWaiting, l.Readiness)
	assert.Empty(t, l.PeerChecks)

	since := *l.SyncedSince

	b.advance(3 * time.Minute)
	b.step()

	// Two references gain peers: the median is 33 now, 17 are needed. The
	// host has not synced since.
	b.get("ref1").Status.Node = nodeWith(33, 33)
	b.get("ref2").Status.Node = nodeWith(33, 33)

	l = b.seen(nodeWith(16, 16), "a")
	require.NotNil(t, l)
	assert.Equal(t, noteNodeWaiting, l.Readiness, "not compared before the run is complete")
	require.NotNil(t, l.SyncedSince)
	assert.Equal(t, since, *l.SyncedSince, "the run is not ended")

	// The epoch is over: now it is compared, and falls short by one.
	b.advance(testEpoch - 3*time.Minute - 5*time.Second)
	b.step()

	l = b.lease("a")
	require.NotNil(t, l, "not released below the share")
	assert.Equal(t, fleet.LeaseVerifying, l.State)
	assert.Equal(t, "EL peers 16, the fleet's median is 33 (needs 17)", l.Readiness)
	require.NotNil(t, l.SyncedSince)
	assert.Equal(t, since, *l.SyncedSince, "falling short does not end the run either")

	// It holds for as long as the host is short.
	b.advance(time.Minute)
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Equal(t, since, *b.lease("a").SyncedSince)

	// One more peer: released in that reconcile, not an epoch later.
	b.get("a").Status.Node = nodeWith(17, 17)
	b.advance(5 * time.Second)

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, noteReadyNode, releasedReadiness(t, out))

	t.Run("or the fleet comes down to it", func(t *testing.T) {
		b := comparing(t, 0.5, nil, nodeWith(33, 33), nodeWith(33, 33), nodeWith(31, 31))

		l := b.seen(nodeWith(16, 16))
		require.NotNil(t, l)
		assert.Equal(t, "EL peers 16, the fleet's median is 33 (needs 17)", l.Readiness)

		b.get("ref1").Status.Node = nodeWith(31, 31)
		require.Nil(t, b.seen(nodeWith(16, 16), "a"), "released without a new sync of the host")
	})
}

// A host that stays below the fleet fails readiness like any host that does
// not come back, and halts its rollout with the comparison as the reason.
func TestPeerShareBelowFailsAfterTheReadinessTimeout(t *testing.T) {
	b := comparing(t, 0.5, func(b *worldBuilder) {
		b.host("z", 0)
		b.wants("z", packagesNeed("2.0"))
	}, nodeWith(31, 31), nodeWith(31, 31), nodeWith(31, 31))

	for range 31 {
		b.advance(55 * time.Second)
		b.seen(nodeWith(31, 4))
	}

	require.Nil(t, b.lease("a"))

	failed := b.lastLease("a")
	assert.Equal(t, fleet.LeaseFailed, failed.State)
	assert.Equal(t, "not ready after 30m0s: CL peers 4, the fleet's median is 31 (needs 16)", failed.Error)

	r := b.mustRollout(packagesKey("2.0"))
	assert.Equal(t, fleet.RolloutHalted, r.State)
	assert.Equal(t, "a: "+failed.Error, r.Reason)
}

// Fewer than three reference hosts for a client: that client is not compared
// (the fixed number of peers still is). Catches: apply with fewer than three
// references.
func TestPeerShareNeedsThreeReferences(t *testing.T) {
	b := comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31))

	require.Nil(t, b.seen(nodeWith(4, 4)), "two references are no fleet")
	assert.Empty(t, b.lastLease("a").PeerChecks)

	// Three hosts probe the CL, two of them the EL as well.
	clOnly := protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Peers: 31}}

	t.Run("per client", func(t *testing.T) {
		b := comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), clOnly)

		require.Nil(t, b.seen(nodeWith(4, 31)), "two EL references: the EL is not compared")
		assert.Equal(t, []fleet.PeerCheck{{Layer: "CL", Peers: 31, Median: 31, Needs: 16}}, b.lastLease("a").PeerChecks)

		b = comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), clOnly)

		l := b.seen(nodeWith(4, 15))
		require.NotNil(t, l)
		assert.Equal(t, "CL peers 15, the fleet's median is 31 (needs 16)", l.Readiness)
	})

	t.Run("a client the host does not probe is not compared", func(t *testing.T) {
		b := comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), nodeWith(31, 31))

		require.Nil(t, b.seen(protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Peers: 16}}))
		assert.Equal(t, []fleet.PeerCheck{{Layer: "CL", Peers: 16, Median: 31, Needs: 16}}, b.lastLease("a").PeerChecks)

		b = comparing(t, 0.5, nil, nodeWith(31, 31), nodeWith(31, 31), nodeWith(31, 31))

		require.Nil(t, b.seen(protocol.NodeHealth{}), "no probe, nothing to compare")
		assert.Empty(t, b.lastLease("a").PeerChecks)
		assert.Equal(t, "ready: no node probe configured", b.lastLease("a").Readiness)
	})
}

// held gives the host an open lease that its agent works on.
func (b *worldBuilder) held(host string) {
	id := "held-" + host
	b.w.Leases = append(b.w.Leases, &fleet.Lease{
		ID: id, Host: host, State: fleet.LeaseRunning, Rollouts: map[string]string{}, GrantedBy: actorScheduler,
		GrantedAt: b.w.Now, ExpiresAt: b.w.Now.Add(time.Hour),
	})

	st := *b.get(host).Status
	st.Operation = &protocol.Operation{LeaseID: id, Phase: protocol.PhaseApplying, Disrupted: true}
	b.get(host).Status = &st
}

// The median is that of the reference hosts: hosts that hold a lease, are
// broken (for whatever group, not only the rollout's), are stale or whose
// node is not at head with the required peers are not among them, whatever
// they report. Catches: compare against all hosts including the leased ones;
// broken for the rollout's group only; references below the required peers.
func TestPeerShareMedianIsOfTheReferenceHosts(t *testing.T) {
	loud := []string{"x1", "x2", "x3", "x4"}

	for name, exclude := range map[string]func(b *worldBuilder){
		"hosts that hold a lease": func(b *worldBuilder) {
			for _, host := range loud {
				b.held(host)
			}
		},
		// The rollout is about the packages; these hosts are broken for an image.
		"hosts that are broken for another group": func(b *worldBuilder) {
			for _, host := range loud {
				b.broke(host, imageNeed("c", imgGeth, digestA))
			}
		},
		"hosts that are broken for the rollout's group": func(b *worldBuilder) {
			for _, host := range loud {
				b.broke(host, packagesNeed("1.5"))
			}
		},
		"hosts whose node is not at head": func(b *worldBuilder) {
			b.get("x1").Status.Node.EL.Syncing = true
			b.get("x2").Status.Node.CL.Syncing = true
			b.get("x3").Status.Node.CL.ELOffline = true
			b.get("x4").Status.Node.EL.Reachable = false
		},
		"hosts that are silent": func(b *worldBuilder) {
			b.advance(3*time.Minute, loud...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := comparing(t, 0.5, func(b *worldBuilder) {
				for _, host := range loud {
					b.host(host, 0).Status.Node = nodeWith(100, 100)
				}

				exclude(b)
			}, nodeWith(10, 10), nodeWith(10, 10), nodeWith(10, 10))

			// With the four loud hosts the median of seven would be 100.
			require.Nil(t, b.seen(nodeWith(5, 5), loud...))
			assert.Equal(t, []fleet.PeerCheck{
				{Layer: "EL", Peers: 5, Median: 10, Needs: 5},
				{Layer: "CL", Peers: 5, Median: 10, Needs: 5},
			}, b.lastLease("a").PeerChecks)
		})
	}

	// The same four hosts as references: they count.
	b := comparing(t, 0.5, func(b *worldBuilder) {
		for _, host := range loud {
			b.host(host, 0).Status.Node = nodeWith(100, 100)
		}
	}, nodeWith(10, 10), nodeWith(10, 10), nodeWith(10, 10))

	l := b.seen(nodeWith(49, 60))
	require.NotNil(t, l)
	assert.Equal(t, "EL peers 49, the fleet's median is 100 (needs 50)", l.Readiness)

	t.Run("hosts with fewer than the required peers", func(t *testing.T) {
		// Seven nodes ask for 3 peers; four hosts have 2. With them the median
		// of seven would be 2, and a host with 3 would pass.
		b := comparing(t, 0.5, func(b *worldBuilder) {
			b.w.Settings.MinPeers = 3

			for _, host := range []string{"q1", "q2", "q3", "q4"} {
				b.host(host, 0).Status.Node = nodeWith(2, 2)
			}
		}, nodeWith(10, 10), nodeWith(10, 10))

		require.Nil(t, b.seen(nodeWith(3, 3)), "two references: not compared")

		b = comparing(t, 0.5, func(b *worldBuilder) {
			b.w.Settings.MinPeers = 3

			for _, host := range []string{"q1", "q2", "q3", "q4"} {
				b.host(host, 0).Status.Node = nodeWith(2, 2)
			}
		}, nodeWith(10, 10), nodeWith(10, 10), nodeWith(10, 10))

		l := b.seen(nodeWith(3, 3))
		require.NotNil(t, l)
		assert.Equal(t, "EL peers 3, the fleet's median is 10 (needs 5)", l.Readiness)
	})

	t.Run("of an even number, the lower middle value", func(t *testing.T) {
		b := comparing(t, 0.5, nil, nodeWith(10, 10), nodeWith(20, 20), nodeWith(30, 30), nodeWith(40, 40))

		l := b.seen(nodeWith(10, 9))
		require.NotNil(t, l)
		assert.Equal(t, "CL peers 9, the fleet's median is 20 (needs 10)", l.Readiness)
	})

	t.Run("the host itself is no reference", func(t *testing.T) {
		// Three others at 10; the host reports 100 and is not counted in.
		b := comparing(t, 1, nil, nodeWith(10, 10), nodeWith(10, 10), nodeWith(10, 10))

		require.Nil(t, b.seen(nodeWith(100, 100)))
		assert.Equal(t, []fleet.PeerCheck{
			{Layer: "EL", Peers: 100, Median: 10, Needs: 10},
			{Layer: "CL", Peers: 100, Median: 10, Needs: 10},
		}, b.lastLease("a").PeerChecks)
	})
}

// The reference hosts are those of the moment the reconcile begins, whatever
// it does first: a host whose lease the same reconcile releases a moment
// earlier is no reference for the lease it looks at next. Catches: a
// reference set that depends on the order in which leases are looked at.
func TestPeerShareReferencesAreThoseTheReconcileBeginsWith(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.ReadyPeerShare = 0.5
	b.w.Settings.ReadyEpochs = 0
	b.w.Settings.MinPeers = 1
	b.w.Settings.MaxUnavailablePercent = 100

	// Two rollouts of one host each: both are granted at once.
	b.host("a", 0)
	b.wants("a", packagesNeed("2.0"))
	b.host("b", 0)
	b.wants("b", rebootNeed())

	b.host("ref1", 0).Status.Node = nodeWith(31, 31)
	b.host("ref2", 0).Status.Node = nodeWith(31, 31)

	b.step()
	require.Equal(t, []string{"a", "b"}, b.leasedHosts())

	// The lease that the pass looks at first, and the one after it.
	first, second := "a", "b"
	if la, lb := b.lease("a"), b.lease("b"); la.GrantedAt.After(lb.GrantedAt) ||
		(la.GrantedAt.Equal(lb.GrantedAt) && la.ID > lb.ID) {
		first, second = "b", "a"
	}

	b.finish(first)
	b.finish(second)
	b.step()

	// The first comes back with the fleet's peers and is released. Were it a
	// reference from then on, there would be three, and the second, with 4
	// peers, would fall short of 16.
	b.get(first).Status.Node = nodeWith(31, 31)
	b.get(second).Status.Node = nodeWith(4, 4)
	b.advance(5 * time.Second)
	b.step()

	assert.Empty(t, b.leasedHosts(), "two references: neither host is compared in this reconcile")
	assert.Empty(t, b.lastLease(second).PeerChecks)
}

// A forced lease is released when the agent is done: nothing is compared.
// Catches: apply under force.
func TestPeerShareNotUnderForce(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.ReadyPeerShare = 0.5
	b.w.Settings.MinPeers = 1

	b.host("a", 0).Status.Node = nodeWith(4, 4)
	b.wants("a", packagesNeed("2.0"))

	for _, name := range []string{"ref1", "ref2", "ref3"} {
		b.host(name, 0).Status.Node = nodeWith(31, 31)
	}

	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())
	b.lease("a").Force = true

	b.finish("a")

	out := b.step()
	require.Nil(t, b.lease("a"), "released at once")
	assert.Contains(t, auditActions(out), "lease.released")
	assert.Empty(t, b.lastLease("a").PeerChecks)
	assert.Empty(t, b.lastLease("a").Readiness)
}

// Peer counts enter the comparison between 0 and a million: whatever an
// agent reports, the arithmetic cannot overflow or go negative.
func TestPeerShareBoundsWhatAgentsReport(t *testing.T) {
	huge := nodeWith(math.MaxInt, math.MaxInt)

	b := comparing(t, 1, nil, huge, huge, huge)

	l := b.seen(nodeWith(999_999, math.MaxInt))
	require.NotNil(t, l)
	assert.Equal(t, "EL peers 999999, the fleet's median is 1000000 (needs 1000000)", l.Readiness)

	require.Nil(t, b.seen(huge))
	assert.Equal(t, []fleet.PeerCheck{
		{Layer: "EL", Peers: 1_000_000, Median: 1_000_000, Needs: 1_000_000},
		{Layer: "CL", Peers: 1_000_000, Median: 1_000_000, Needs: 1_000_000},
	}, b.lastLease("a").PeerChecks)

	assert.Equal(t, 0, comparedPeers(-5))
	assert.Equal(t, 0, comparedPeers(math.MinInt))
}

// peerShareTrace plays one host through its readiness check next to six
// others with many more peers and returns everything the reconciles wrote.
func peerShareTrace(t *testing.T, tune func(s *fleet.Settings)) string {
	t.Helper()

	b := verifying(t, 7, 0)
	if tune != nil {
		tune(&b.w.Settings)
	}

	var trace strings.Builder

	for _, node := range []protocol.NodeHealth{
		nodeWith(0, 9), nodeWith(2, 2), nodeWith(3, 3), nodeWith(3, 2), nodeWith(3, 3), nodeWith(4, 3), nodeWith(4, 3),
	} {
		b.get("a").Status.Node = node
		b.advance(testEpoch / 2)

		out := b.step()

		raw, err := json.Marshal([]any{out.Rollouts, out.Leases, out.Audit, out.Plan.Queue})
		require.NoError(t, err)

		trace.Write(raw)
		trace.WriteByte('\n')
	}

	require.Nil(t, b.lease("a"), "released in the end")

	return trace.String()
}

// goldenShareOff is the SHA-256 of that trace as the build before the
// setting wrote it (commit b6c4a48).
const goldenShareOff = "96b36ec82ae1d3c9841b18420a30bc0eecb364decb83c1bcbb90d987ef685f9e"

// With readyPeerShare 0, which is the default, the readiness check is the
// one of the build before: the same notes, the same rows, byte for byte, next
// to hosts with three times the peers. Catches: a comparison that is on by
// default, or cannot be turned off.
func TestPeerShareZeroIsTheBuildBefore(t *testing.T) {
	assert.Zero(t, fleet.DefaultSettings().ReadyPeerShare, "off unless somebody turns it on")

	for name, tune := range map[string]func(s *fleet.Settings){
		"the defaults": func(s *fleet.Settings) { s.ReadyPeerShare = fleet.DefaultSettings().ReadyPeerShare },
		"set to 0":     func(s *fleet.Settings) { s.ReadyPeerShare = 0 },
	} {
		off := peerShareTrace(t, tune)
		sum := sha256.Sum256([]byte(off))

		assert.Equal(t, goldenShareOff, hex.EncodeToString(sum[:]), name)
		assert.NotContains(t, off, "peerChecks", name)
		assert.NotContains(t, off, "median", name)
	}

	// Turned on, the same history stops at the comparison once the node has
	// been at head for its epoch.
	b := verifying(t, 7, 0)
	b.w.Settings.ReadyPeerShare = 0.5
	b.get("a").Status.Node = nodeWith(3, 3)
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Equal(t, noteNodeWaiting, b.lease("a").Readiness)

	b.advance(testEpoch)
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Equal(t, "EL peers 3, the fleet's median is 9 (needs 5)", b.lease("a").Readiness)
}

// When every host is updated and peers are low everywhere, the median is low
// too: in a fleet of hosts alike the comparison holds nobody back.
func TestPeerShareLowPeersEverywhereStillRelease(t *testing.T) {
	b := comparing(t, 1, func(b *worldBuilder) {
		for _, name := range []string{"ref1", "ref2", "ref3", "ref4"} {
			b.wants(name, packagesNeed("2.0"))
		}
	}, nodeWith(2, 2), nodeWith(2, 2), nodeWith(2, 2), nodeWith(2, 2))

	// Even at share 1 a host that has what the others have is ready.
	for _, host := range []string{"a", "ref1", "ref2", "ref3", "ref4"} {
		if b.lease(host) == nil {
			b.advance(5 * time.Second)
			b.step()
		}

		require.NotNil(t, b.lease(host), "%s is granted", host)

		if b.lease(host).State != fleet.LeaseVerifying {
			b.finish(host)
			b.step()
		}

		b.get(host).Status.Node = nodeWith(2, 2)
		b.advance(5 * time.Second)
		b.step()
		require.Nil(t, b.lease(host), "%s is released", host)
	}

	assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)
}
