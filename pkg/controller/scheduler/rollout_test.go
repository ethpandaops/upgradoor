package scheduler

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// skippedFleet is a packages rollout in which "a" was served and "b" was
// skipped by a person: only the skipped host remains.
func skippedFleet(t *testing.T) (h *harness, rollout string) {
	t.Helper()

	h = newHarness(t)
	h.addHost("a", 0, packagesNeed("2.0"))
	h.addHost("b", 0, packagesNeed("2.0"))
	h.tick()

	rollout = h.openRollouts()[0].ID

	_, err := h.svc.Skip(h.ctx, "alice", RolloutHostsRequest{Rollout: rollout, Hosts: []string{"b"}, Reason: "disk"})
	require.NoError(t, err)

	h.finish("a")
	require.Equal(t, fleet.HostDone, h.rollout(rollout).Host("a").State)

	return h, rollout
}

// A skip in a rollout of an observed cause holds for as long as the host has
// the need, and keeps the rollout open that long: a rollout that completed
// would be opened again for the host with its next sync. No closed rollout is
// read to remember it.
func TestSkipKeepsTheRolloutOpenWhileTheHostHasTheNeed(t *testing.T) {
	h, id := skippedFleet(t)

	stays := func(t *testing.T) {
		t.Helper()

		for range 3 {
			h.advance(time.Minute)
			h.tick()
		}

		r := h.rollout(id)
		assert.Equal(t, fleet.RolloutActive, r.State)
		assert.Equal(t, fleet.HostSkipped, r.Host("b").State)
		assert.Equal(t, "alice", r.Host("b").SkippedBy)
		assert.Equal(t, "disk", r.Host("b").Error)
		assert.Len(t, h.openRollouts(), 1, "no second rollout for the same cause")
		assert.Nil(t, h.assignment("b").Lease)
		assert.Nil(t, h.assignment("b").Queue, "a skipped host does not wait")
	}

	stays(t)

	// A restarted scheduler finds the skip where it is: in the open rollout.
	h.restart()
	stays(t)

	// A held host reports no needs. That says nothing about the need.
	h.sync("b", func(st *protocol.SyncRequest) {
		st.Hold.Local = true
		st.Needs = nil
	})
	h.tick()
	assert.Equal(t, fleet.HostSkipped, h.rollout(id).Host("b").State)
	assert.Equal(t, fleet.RolloutActive, h.rollout(id).State)

	// The hold is lifted and the need is gone (somebody upgraded the host by
	// hand): the member is done like a waiting one, and the rollout completes.
	h.sync("b", func(st *protocol.SyncRequest) { st.Hold.Local = false })
	h.tick()

	r := h.rollout(id)
	assert.Equal(t, fleet.HostDone, r.Host("b").State)
	assert.Empty(t, r.Host("b").SkippedBy)
	assert.Empty(t, r.Host("b").Error)
	assert.Equal(t, fleet.RolloutCompleted, r.State)
}

func TestSkipIsTakenBackOrGoesWithItsHost(t *testing.T) {
	t.Run("a retry that names the host takes the skip back", func(t *testing.T) {
		h, id := skippedFleet(t)

		_, err := h.svc.Retry(h.ctx, "bob", RolloutHostsRequest{Rollout: id})
		require.ErrorIs(t, err, ErrInvalidAction, "a retry of the rollout is about failed hosts")

		res, err := h.svc.Retry(h.ctx, "bob", RolloutHostsRequest{Rollout: id, Hosts: []string{"b"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"b"}, res.Hosts)

		h.tick()
		assert.NotNil(t, h.assignment("b").Lease)
		assert.Empty(t, h.rollout(id).Host("b").SkippedBy)
	})

	t.Run("a forgotten host keeps no rollout open", func(t *testing.T) {
		h, id := skippedFleet(t)

		_, err := h.svc.Forget(h.ctx, "alice", HoldRequest{Hosts: []string{"b"}})
		require.NoError(t, err)
		h.tick()

		r := h.rollout(id)
		assert.Equal(t, fleet.RolloutCompleted, r.State)
		assert.Equal(t, fleet.HostSkipped, r.Host("b").State)
		assert.Equal(t, reasonHostForgotten, r.Host("b").Error)
		assert.Empty(t, r.Host("b").SkippedBy)
	})

	t.Run("a newer cause supersedes the rollout, and the skip goes with it", func(t *testing.T) {
		h, id := skippedFleet(t)

		h.sync("b", func(st *protocol.SyncRequest) { st.Needs = []protocol.Need{packagesNeed("3.0")} })
		h.tick()

		assert.Equal(t, fleet.RolloutSuperseded, h.rollout(id).State)
		require.Len(t, h.openRollouts(), 1)
		assert.NotNil(t, h.assignment("b").Lease, "the skip was for the cause that is gone")
	})

	t.Run("in a manual rollout a skip is final", func(t *testing.T) {
		h := newHarness(t)
		h.addHost("a", 0)
		h.addHost("b", 0)

		res, err := h.svc.Reboot(h.ctx, "alice", RebootRequest{All: true, Staggered: true})
		require.NoError(t, err)

		_, err = h.svc.Skip(h.ctx, "alice", RolloutHostsRequest{Rollout: res.Rollout, Hosts: []string{"a", "b"}})
		require.NoError(t, err)
		h.tick()

		assert.Equal(t, fleet.RolloutCompleted, h.rollout(res.Rollout).State)
		assert.Empty(t, h.assignment("a").Directives, "its unanswered directives are cancelled")
	})
}

// A host that cannot fit the budget is not the canary while another host fits.
func TestCanaryThatCannotFitTheBudgetDoesNotStallTheRollout(t *testing.T) {
	key := packagesKey("2.0")

	// A: no validator may go down at all.
	t.Run("maxUnavailablePercent 0", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 0
		b.host("tool1", 0)
		b.host("tool2", 0)
		b.host("val", 10)

		for _, h := range b.w.Hosts {
			b.wants(h.Name, packagesNeed("2.0"))
		}

		out := b.step()
		assert.Equal(t, []string{"tool1"}, b.leasedHosts())
		assert.Equal(t, "tool1", b.mustRollout(key).Canary)
		assert.Equal(t, map[string]string{
			"tool2": "after canary tool1", "val": "after canary tool1",
		}, queueReasons(out.Plan))

		b.serve("tool1")
		b.serve("tool2")

		out = b.step()
		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, map[string]string{"val": "budget 0% of 0%"}, queueReasons(out.Plan))
	})

	// B: a host outside the rollout is down for good and uses the budget up.
	t.Run("the budget is used up by a host outside the rollout", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("dead", 10)
		b.liveOverride["dead"] = 0
		b.host("val", 50)
		b.host("val2", 40)
		b.host("tool", 0)

		for _, h := range []string{"val", "val2", "tool"} {
			b.wants(h, packagesNeed("2.0"))
		}

		out := b.step()
		require.True(t, out.Plan.Gate.OK)
		assert.Equal(t, []string{"tool"}, b.leasedHosts())
		assert.Equal(t, "tool", b.mustRollout(key).Canary)
		assert.Equal(t, map[string]string{
			"val": "after canary tool", "val2": "after canary tool",
		}, queueReasons(out.Plan))

		b.serve("tool")

		out = b.step()
		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, map[string]string{
			"val": "budget 10% of 10%", "val2": "budget 10% of 10%",
		}, queueReasons(out.Plan))
	})

	// When nothing fits, the first candidate is the canary and waits for the
	// budget; a host that fits later takes its place.
	t.Run("nothing fits", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("dead", 10)
		b.liveOverride["dead"] = 0
		b.host("val", 50)
		b.host("val2", 40)
		b.wants("val", packagesNeed("2.0"))
		b.wants("val2", packagesNeed("2.0"))

		out := b.step()
		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, "val2", b.mustRollout(key).Canary, "the lighter one")
		assert.Equal(t, map[string]string{
			"val2": "budget 10% of 10%", "val": "after canary val2",
		}, queueReasons(out.Plan))

		b.host("tool", 0)
		b.wants("tool", packagesNeed("2.0"))

		out = b.step()
		assert.Equal(t, []string{"tool"}, b.leasedHosts())
		assert.Equal(t, "tool", b.mustRollout(key).Canary)
		assert.Equal(t, "after canary tool", queueReasons(out.Plan)["val2"])
	})

	// With room in the budget the canary is still a host with validators.
	t.Run("a host with validators that fits is preferred", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("tool", 0)
		b.host("val", 10)
		b.host("idle", 90)
		b.wants("tool", packagesNeed("2.0"))
		b.wants("val", packagesNeed("2.0"))

		b.step()
		assert.Equal(t, []string{"val"}, b.leasedHosts())
		assert.Equal(t, "val", b.mustRollout(key).Canary)
	})
}

// A host whose validators never attest, with a working agent, is an ordinary
// member.
func TestHostWithoutLiveValidatorsIsNeitherFirstNorCanary(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.MinParticipationPercent = 50

	for _, name := range []string{"dead", "x", "y", "z"} {
		b.host(name, 32)
		b.wants(name, packagesNeed("2.0"))
	}

	b.down("dead")

	out := b.step()
	r := b.mustRollout(packagesKey("2.0"))

	require.True(t, out.Plan.Gate.OK)
	assert.Equal(t, []string{"x"}, b.leasedHosts(), "a host with live validators is the canary")
	assert.Equal(t, "x", r.Canary)
	assert.Equal(t, fleet.StageRest, r.Host("dead").Stage)
	assert.Equal(t, "after canary x", queueReasons(out.Plan)["dead"])

	// After the first success it takes its turn in the rest.
	b.serve("x")
	assert.Equal(t, []string{"dead", "y", "z"}, b.leasedHosts())
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

func TestRolloutOpensPerCause(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.host("b", 0)
	b.wantsImage("a", imgLighthouse, digestA)
	b.wantsImage("b", imgLighthouse, digestA)
	b.wants("a", packagesNeed("2.0"), rebootNeed())

	out := b.step()

	require.Len(t, b.w.Rollouts, 3)

	img := b.mustRollout(imageKey("", imgLighthouse, digestA))
	assert.Equal(t, fleet.RolloutImage, img.Kind)
	assert.Equal(t, imgLighthouse, img.Image)
	assert.Equal(t, digestA, img.Digest)
	assert.Equal(t, fleet.RolloutActive, img.State)
	assert.Equal(t, actorScheduler, img.CreatedBy)
	assert.Len(t, img.Hosts, 2)

	pkgs := b.mustRollout(packagesKey("2.0"))
	assert.Equal(t, fleet.RolloutPackages, pkgs.Kind)
	assert.Equal(t, "packages: docker-ce 2.0", pkgs.Title)
	assert.NotNil(t, b.rollout("reboot"))

	assert.Subset(t, auditActions(out), []string{"rollout.created", "lease.granted"})

	// All needs of a host go into one lease.
	l := b.lease("a")
	require.NotNil(t, l)
	assert.Len(t, l.Needs, 3)
	assert.Len(t, l.Rollouts, 3)
	assert.Equal(t, actorScheduler, l.GrantedBy)
	assert.Equal(t, b.w.Now.Add(30*time.Minute), l.ExpiresAt)

	// Nothing new on an unchanged world: at most one open rollout per key.
	b.step()
	assert.Len(t, b.w.Rollouts, 3)
}

func TestImageSupersedeCarriesFailedHostsFirst(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100

	for _, h := range []string{"h1", "h2", "h3"} {
		b.host(h, 10)
		b.wantsImage(h, imgLighthouse, digestA)
	}

	// "h3" would be the last candidate by name; make it the one that fails.
	b.get("h1").HoldWanted = true
	b.get("h2").HoldWanted = true
	b.step()
	require.Equal(t, []string{"h3"}, b.leasedHosts())
	b.report("h3", protocol.PhaseFailed, true)
	b.get("h1").HoldWanted = false
	b.get("h2").HoldWanted = false
	b.step()

	first := b.mustRollout(imageKey("", imgLighthouse, digestA))
	require.Equal(t, fleet.RolloutHalted, first.State)
	require.Empty(t, b.leasedHosts(), "the digest is blocked")

	// The tag moves: "h1" is the first to see the new digest.
	b.advance(time.Minute)

	h1 := b.get("h1")
	h1.Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestB, b.w.Now)}
	h1.Status.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digestB)}

	out := b.step()

	assert.Nil(t, b.rollout(first.Key))
	assert.Equal(t, fleet.RolloutSuperseded, b.closed(first.Key).State)
	assert.Contains(t, auditActions(out), "rollout.superseded")

	second := b.mustRollout(imageKey("", imgLighthouse, digestB))
	assert.Equal(t, map[string]fleet.HostState{
		"h1": fleet.HostPending, "h2": fleet.HostExpected, "h3": fleet.HostExpected,
	}, memberStates(second), "nothing is granted before the host the change broke had its turn")
	assert.Equal(t, fleet.StageRepair, second.Host("h3").Stage)
	assert.Equal(t, map[string]string{"h1": "after h3"}, queueReasons(out.Plan))

	// The expected hosts are told to look at the registry now.
	rechecks := map[string]bool{}

	for _, d := range b.w.Directives {
		if d.Kind == protocol.DirectiveRecheck && d.State == fleet.DirectivePending {
			rechecks[d.Host] = true
		}
	}

	assert.Equal(t, map[string]bool{"h2": true, "h3": true}, rechecks)
	// Hosts that still report the old digest do not reopen it.
	b.step()
	assert.Len(t, b.w.Rollouts, 1)

	// Everyone sees the new digest: the host that had failed goes first,
	// although "h1" and "h2" sort before it.
	b.advance(time.Minute)

	for _, name := range []string{"h2", "h3"} {
		h := b.get(name)
		h.Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestB, b.w.Now)}
		h.Status.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digestB)}
	}

	out = b.step()
	second = b.mustRollout(second.Key)
	assert.Equal(t, []string{"h3"}, b.leasedHosts())
	assert.Equal(t, fleet.StageRepair, second.Host("h3").Stage)
	assert.Empty(t, second.Canary, "no canary while a broken host is being repaired")
	assert.Equal(t, map[string]string{"h1": "after h3", "h2": "after h3"}, queueReasons(out.Plan))

	// The repair is the first success: nobody waits for a canary after it.
	b.serve("h3")
	second = b.mustRollout(second.Key)
	assert.Equal(t, []string{"h1", "h2"}, b.leasedHosts())
	assert.Empty(t, second.Canary)
	assert.Equal(t, fleet.StageRepair, second.Host("h3").Stage, "a served member keeps its stage")
}

func TestImageTargetIsTheFreshestRegistryCheck(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("stale-view", 0)
	b.host("fresh-view", 0)

	old := b.get("stale-view")
	old.Status.Containers = []protocol.Container{container("c", imgGeth, digestOld, digestA, b.w.Now.Add(-time.Hour))}
	old.Status.Needs = []protocol.Need{imageNeed("c", imgGeth, digestA)}

	fresh := b.get("fresh-view")
	fresh.Status.Containers = []protocol.Container{container("c", imgGeth, digestOld, digestB, b.w.Now)}
	fresh.Status.Needs = []protocol.Need{imageNeed("c", imgGeth, digestB)}

	b.step()

	require.Len(t, b.w.Rollouts, 1)
	r := b.mustRollout(imageKey("", imgGeth, digestB))
	assert.Equal(t, map[string]fleet.HostState{
		"fresh-view": fleet.HostLeased, "stale-view": fleet.HostExpected,
	}, memberStates(r))

	// Stable: no rollout for the older digest appears on later passes.
	b.step()
	b.step()
	assert.Len(t, b.w.Rollouts, 1)
	assert.Empty(t, b.closedRollouts)
}

func TestPackagesSupersede(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.host("b", 0)
	b.get("a").HoldWanted = true // keep both waiting
	b.get("b").HoldWanted = true
	b.wants("a", packagesNeed("2.0"))
	b.wants("b", packagesNeed("2.0"))
	b.step()

	v2 := packagesKey("2.0")
	v3 := packagesKey("3.0")
	require.NotNil(t, b.rollout(v2))

	// One host sees newer versions, the other still reports the old set:
	// both causes are open.
	b.get("a").Status.Needs = []protocol.Need{packagesNeed("3.0")}
	b.step()
	assert.NotNil(t, b.rollout(v2))
	assert.NotNil(t, b.rollout(v3))

	// Nobody reports the old set any more.
	b.get("b").Status.Needs = []protocol.Need{packagesNeed("3.0")}
	b.step()
	assert.Nil(t, b.rollout(v2))
	assert.Equal(t, fleet.RolloutSuperseded, b.closed(v2).State)
	assert.Len(t, b.mustRollout(v3).Hosts, 2)
}

func TestPackagesRolloutCompletesWhenNeedsVanish(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.get("a").HoldWanted = true
	b.wants("a", packagesNeed("2.0"))
	b.step()

	b.get("a").Status.Needs = nil
	out := b.step()

	assert.Empty(t, b.w.Rollouts)
	assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)
	assert.Equal(t, fleet.HostDone, b.closed(packagesKey("2.0")).Host("a").State)
	assert.Contains(t, auditActions(out), "rollout.completed")
}

func TestMembership(t *testing.T) {
	key := imageKey("", imgLighthouse, digestA)

	t.Run("an expected host joins when it reports and is done when someone fixed it", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("reporter", 0)
		b.host("slow", 0)
		b.host("fixed", 0)
		b.host("custom", 0)
		b.host("elsewhere", 0)
		b.wantsImage("reporter", imgLighthouse, digestA)
		b.get("slow").Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestOld, b.w.Now)}
		b.get("fixed").Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestOld, b.w.Now)}

		custom := container("c", "someone/debug:build", digestOld, "", b.w.Now)
		custom.ConfiguredImage = imgLighthouse
		b.get("custom").Status.Containers = []protocol.Container{custom}
		b.get("elsewhere").Status.Containers = []protocol.Container{container("c", imgGeth, digestOld, digestOld, b.w.Now)}

		b.step()

		r := b.mustRollout(key)
		assert.Equal(t, map[string]fleet.HostState{
			"reporter": fleet.HostLeased, "slow": fleet.HostExpected, "fixed": fleet.HostExpected,
		}, memberStates(r), "custom containers and other images are not expected")

		b.wantsImage("slow", imgLighthouse, digestA)
		b.get("fixed").Status.Containers = []protocol.Container{container("c", imgLighthouse, digestA, digestA, b.w.Now)}
		b.step()

		r = b.mustRollout(key)
		assert.Equal(t, fleet.HostPending, r.Host("slow").State)
		assert.Equal(t, fleet.HostDone, r.Host("fixed").State)
	})

	t.Run("an expected host with validators is waited for as the canary only for hostStaleAfter", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("tool", 0)
		b.host("val", 10)
		b.wantsImage("tool", imgLighthouse, digestA)
		b.get("val").Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestOld, b.w.Now)}

		// The host with validators is the canary as soon as it reports.
		out := b.step()
		assert.Equal(t, "val", b.mustRollout(key).Canary)
		assert.Equal(t, "after canary val", queueReasons(out.Plan)["tool"])

		b.advance(b.w.Settings.HostStaleAfter.Std() + time.Second)
		b.step()
		assert.Equal(t, []string{"tool"}, b.leasedHosts())
		assert.Equal(t, fleet.HostExpected, b.mustRollout(key).Host("val").State, "it is still shown as expected")
		assert.Empty(t, b.mustRollout(key).Canary, "one member left to serve: no canary")

		// The rollout completes without it.
		b.serve("tool")
		assert.Equal(t, fleet.RolloutCompleted, b.closed(key).State)
	})

	t.Run("an expected host that is held, stale or lacks the verb is not waited for", func(t *testing.T) {
		for _, tt := range []struct {
			name  string
			setup func(h *fleet.Host, now time.Time)
		}{
			{"held", func(h *fleet.Host, _ time.Time) { h.Status.Hold.Local = true }},
			{"stale", func(h *fleet.Host, now time.Time) { h.LastSeen = now.Add(-time.Hour) }},
			{"verb", func(h *fleet.Host, _ time.Time) { h.Status.Verbs = []protocol.Verb{protocol.VerbReboot} }},
		} {
			t.Run(tt.name, func(t *testing.T) {
				b := newWorldBuilder(t)
				b.host("tool", 0)
				b.host("val", 10)
				b.wantsImage("tool", imgLighthouse, digestA)
				b.get("val").Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestOld, b.w.Now)}
				tt.setup(b.get("val"), b.w.Now)

				b.step()

				assert.Equal(t, []string{"tool"}, b.leasedHosts())
			})
		}
	})

	t.Run("a held host leaves the queue and rejoins when the hold lifts", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.PolicyImage = fleet.PolicyManual
		b.w.Settings.PolicyPackages = fleet.PolicyManual
		b.host("a", 0)
		b.host("b", 0)
		b.wantsImage("a", imgLighthouse, digestA)
		b.wantsImage("b", imgLighthouse, digestA)
		b.wants("a", packagesNeed("2.0"))
		b.wants("b", packagesNeed("2.0"))
		b.step()

		// The agent of "a" obeys a hold: it reports no needs any more.
		a := b.get("a")
		needs := a.Status.Needs
		a.Status.Hold.Local = true
		a.Status.Needs = nil
		out := b.step()

		assert.Equal(t, fleet.HostExpected, b.mustRollout(key).Host("a").State,
			"it still runs the old digest")
		assert.Equal(t, fleet.HostDone, b.mustRollout(packagesKey("2.0")).Host("a").State)
		assert.NotContains(t, queueReasons(out.Plan), "a")
		assert.Equal(t, SlotHeld, out.Plan.Hosts["a"].Slot)

		a.Status.Hold.Local = false
		a.Status.Needs = needs
		b.step()

		assert.Equal(t, fleet.HostPending, b.mustRollout(key).Host("a").State)
		assert.Equal(t, fleet.HostPending, b.mustRollout(packagesKey("2.0")).Host("a").State)
	})

	t.Run("a forgotten host is skipped", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.PolicyPackages = fleet.PolicyManual
		b.host("a", 0)
		b.host("b", 0)
		b.wants("a", packagesNeed("2.0"))
		b.wants("b", packagesNeed("2.0"))
		b.step()

		b.w.Hosts = b.w.Hosts[:1]
		b.step()

		m := b.mustRollout(packagesKey("2.0")).Host("b")
		assert.Equal(t, fleet.HostSkipped, m.State)
		assert.Equal(t, "host forgotten", m.Error)
	})
}

// TestCanaryThenTheRest: with no broken host, the canary alone, then
// everybody else at once. A bootnode is a host like any other.
func TestCanaryThenTheRest(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100

	b.host("tool1", 0)
	b.host("tool2", 0)
	b.host("val-heavy", 20)
	b.host("val-light", 5)
	b.host("val-mid", 10)
	b.host("boot1", 0, "bootnode")
	b.host("boot2", 8, "bootnode")

	for _, h := range b.w.Hosts {
		b.wants(h.Name, packagesNeed("2.0"))
	}

	key := packagesKey("2.0")

	// The canary is the lightest host with validators, alone.
	out := b.step()
	assert.Equal(t, []string{"val-light"}, b.leasedHosts())
	assert.Equal(t, "val-light", b.mustRollout(key).Canary)
	assert.Equal(t, fleet.StageCanary, b.mustRollout(key).Host("val-light").Stage)
	assert.Equal(t, map[string]string{
		"tool1":     "after canary val-light",
		"tool2":     "after canary val-light",
		"boot1":     "after canary val-light",
		"boot2":     "after canary val-light",
		"val-mid":   "after canary val-light",
		"val-heavy": "after canary val-light",
	}, queueReasons(out.Plan))

	order := make([]string, 0, len(out.Plan.Queue))
	for _, q := range out.Plan.Queue {
		assert.Equal(t, WaitOrder, q.Wait)
		assert.Equal(t, "val-light", q.After, "the host it waits for")

		order = append(order, q.Host)
	}

	assert.Equal(t, []string{"boot1", "tool1", "tool2", "boot2", "val-mid", "val-heavy"}, order,
		"by ascending weight and name, whatever the role")

	// Not before the canary is done: a done report alone is not enough.
	b.finish("val-light")
	b.step()
	assert.Equal(t, []string{"val-light"}, b.leasedHosts(), "the canary is still being verified")

	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)

	// The rest: every other member at once, bootnodes among them.
	out = b.step()
	assert.Equal(t, []string{"boot1", "boot2", "tool1", "tool2", "val-heavy", "val-mid"}, b.leasedHosts())
	assert.Empty(t, out.Plan.Queue)

	r := b.mustRollout(key)
	assert.Equal(t, fleet.HostDone, r.Host("val-light").State)
	assert.Equal(t, fleet.StageCanary, r.Host("val-light").Stage)

	for _, name := range b.leasedHosts() {
		assert.Equal(t, fleet.StageRest, r.Host(name).Stage, name)
		b.report(name, protocol.PhaseApplying, true)
	}

	for _, name := range b.leasedHosts() {
		b.serve(name)
	}

	assert.Equal(t, fleet.RolloutCompleted, b.closed(key).State)
}

func TestCanaryChoice(t *testing.T) {
	key := packagesKey("2.0")

	start := func(t *testing.T, hosts map[string]uint64) *worldBuilder {
		t.Helper()

		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 100

		for _, name := range sortedKeys(hosts) {
			b.host(name, hosts[name])
			b.wants(name, packagesNeed("2.0"))
		}

		return b
	}

	t.Run("a host with validators when the rollout has one", func(t *testing.T) {
		b := start(t, map[string]uint64{"a-tool": 0, "b-tool": 0, "val": 40})

		out := b.step()
		assert.Equal(t, []string{"val"}, b.leasedHosts())
		assert.Equal(t, "val", b.mustRollout(key).Canary)
		assert.Equal(t, map[string]string{"a-tool": "after canary val", "b-tool": "after canary val"},
			queueReasons(out.Plan))
	})

	t.Run("the first member when no member has validators", func(t *testing.T) {
		b := start(t, map[string]uint64{"b": 0, "a": 0, "c": 0})

		out := b.step()
		assert.Equal(t, []string{"a"}, b.leasedHosts())
		assert.Equal(t, "a", b.mustRollout(key).Canary)
		assert.Equal(t, map[string]string{"b": "after canary a", "c": "after canary a"}, queueReasons(out.Plan))

		b.serve("a")
		assert.Equal(t, []string{"b", "c"}, b.leasedHosts())
	})

	t.Run("a bootnode is chosen like any host", func(t *testing.T) {
		b := start(t, map[string]uint64{"val": 10})
		b.host("boot", 3, "bootnode")
		b.wants("boot", packagesNeed("2.0"))

		b.step()
		assert.Equal(t, []string{"boot"}, b.leasedHosts(), "the lightest host with validators")
		assert.Equal(t, "boot", b.mustRollout(key).Canary)
	})

	t.Run("a single member needs no canary", func(t *testing.T) {
		b := start(t, map[string]uint64{"only": 10})

		b.step()
		assert.Equal(t, []string{"only"}, b.leasedHosts())
		assert.Empty(t, b.mustRollout(key).Canary)
		assert.Equal(t, fleet.StageRest, b.mustRollout(key).Host("only").Stage)
	})

	t.Run("a member that joins while the first host is out waits for it", func(t *testing.T) {
		b := start(t, map[string]uint64{"only": 0})
		b.step()
		require.Equal(t, []string{"only"}, b.leasedHosts())

		b.host("late", 10)
		b.wants("late", packagesNeed("2.0"))

		out := b.step()
		assert.Equal(t, []string{"only"}, b.leasedHosts())
		assert.Equal(t, "only", b.mustRollout(key).Canary, "the host that went first is the canary")
		assert.Equal(t, "after canary only", queueReasons(out.Plan)["late"])
	})

	t.Run("a validator host that cannot be served is not waited for", func(t *testing.T) {
		for name, setup := range map[string]func(b *worldBuilder){
			"silent":  func(b *worldBuilder) { b.get("val").LastSeen = b.w.Now.Add(-time.Hour) },
			"held":    func(b *worldBuilder) { b.get("val").HoldWanted = true },
			"no verb": func(b *worldBuilder) { b.get("val").Status.Verbs = []protocol.Verb{protocol.VerbRoll} },
		} {
			t.Run(name, func(t *testing.T) {
				b := start(t, map[string]uint64{"tool": 0, "val": 10})
				setup(b)

				b.step()
				assert.Equal(t, []string{"tool"}, b.leasedHosts())
				assert.Equal(t, "tool", b.mustRollout(key).Canary)
			})
		}
	})

	t.Run("a silent canary that was not served is replaced", func(t *testing.T) {
		b := start(t, map[string]uint64{"a": 5, "b": 6})
		b.w.Settings.Paused = true
		b.step()
		require.Equal(t, "a", b.mustRollout(key).Canary)

		b.w.Settings.Paused = false
		b.advance(b.w.Settings.HostStaleAfter.Std()+time.Second, "a")
		b.step()
		assert.Equal(t, "b", b.mustRollout(key).Canary)
		assert.Equal(t, []string{"b"}, b.leasedHosts())
	})
}

func TestCanaryCandidates(t *testing.T) {
	t.Run("a host whose validators are all offline is not preferred", func(t *testing.T) {
		// It proves nothing about attesting; with room in the budget the
		// canary is a host with live validators, however heavy.
		b := newWorldBuilder(t)
		b.w.Settings.MinParticipationPercent = 10
		b.w.Settings.MaxUnavailablePercent = 100
		b.host("down", 5)
		b.host("live", 30)
		b.host("tool", 0)

		for _, h := range b.w.Hosts {
			b.wants(h.Name, packagesNeed("2.0"))
		}

		b.down("down")

		out := b.step()
		r := b.mustRollout(packagesKey("2.0"))

		require.True(t, out.Plan.Gate.OK)
		assert.Equal(t, []string{"live"}, b.leasedHosts())
		assert.Equal(t, "live", r.Canary)
		assert.Equal(t, fleet.StageRest, r.Host("down").Stage, "an ordinary member")
		assert.Equal(t, map[string]string{
			"down": "after canary live", "tool": "after canary live",
		}, queueReasons(out.Plan))

		// Without a host with live validators it ranks with the hosts that
		// have none: the lightest of them, then by name.
		b = newWorldBuilder(t)
		b.w.Settings.MinParticipationPercent = 10
		b.host("down", 5)
		b.host("idle", 95)
		b.host("tool", 0)
		b.wants("down", packagesNeed("2.0"))
		b.wants("tool", packagesNeed("2.0"))
		b.down("down")

		b.step()
		assert.Equal(t, []string{"tool"}, b.leasedHosts())
	})

	t.Run("a broken host that waits for a person holds nobody back", func(t *testing.T) {
		for name, setup := range map[string]func(b *worldBuilder){
			"silent":  func(b *worldBuilder) { b.get("broken").LastSeen = b.w.Now.Add(-time.Hour) },
			"held":    func(b *worldBuilder) { b.get("broken").HoldWanted = true },
			"no verb": func(b *worldBuilder) { b.get("broken").Status.Verbs = []protocol.Verb{protocol.VerbRoll} },
		} {
			t.Run(name, func(t *testing.T) {
				b := newWorldBuilder(t)
				b.w.Settings.MaxUnavailablePercent = 100
				b.host("light", 5)
				b.host("mid", 8)
				b.host("broken", 30)

				for _, h := range b.w.Hosts {
					b.wants(h.Name, packagesNeed("2.0"))
				}

				b.broke("broken", packagesNeed("1.5"))
				setup(b)

				out := b.step()
				r := b.mustRollout(packagesKey("2.0"))

				assert.Equal(t, []string{"light"}, b.leasedHosts())
				assert.Equal(t, "light", r.Canary)
				assert.Equal(t, fleet.StageRepair, r.Host("broken").Stage)
				assert.Equal(t, "after canary light", queueReasons(out.Plan)["mid"])
			})
		}
	})

	t.Run("a broken host that waits for the budget holds the others back", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 100
		b.host("a", 10)
		b.host("b", 10)
		b.host("tool", 0)

		for _, h := range b.w.Hosts {
			b.wants(h.Name, packagesNeed("2.0"))
		}

		// "a" attests, but its last packages lease failed; the budget is zero.
		b.broke("a", packagesNeed("1.5"))
		b.w.Settings.MaxUnavailablePercent = 0

		out := b.step()
		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, map[string]string{
			"a": "budget 0% of 0%", "b": "after a", "tool": "after a",
		}, queueReasons(out.Plan))
	})

	t.Run("a skipped canary is replaced", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 100
		b.host("a", 5)
		b.host("b", 6)
		b.host("c", 7)

		for _, h := range b.w.Hosts {
			b.wants(h.Name, packagesNeed("2.0"))
		}

		b.step()
		require.Equal(t, []string{"a"}, b.leasedHosts())

		// The lease expires unused, then a person skips the canary.
		b.report("a", protocol.PhaseDone, false, protocol.StepRefused)
		b.get("a").HoldWanted = true
		b.step()

		r := b.mustRollout(packagesKey("2.0"))
		r.Host("a").State = fleet.HostSkipped
		b.step()

		assert.Equal(t, []string{"b"}, b.leasedHosts(), "one new canary, alone")
		assert.Equal(t, "b", b.mustRollout(packagesKey("2.0")).Canary)
	})

	t.Run("a canary whose need goes away unserved is replaced", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 100
		b.host("a", 5)
		b.host("b", 6)
		b.host("c", 7)

		for _, h := range b.w.Hosts {
			b.wants(h.Name, packagesNeed("2.0"))
		}

		b.step()
		require.Equal(t, []string{"a"}, b.leasedHosts())

		// The canary refuses the lease (it went on hold) and reports no
		// need any more: it is done without having proved anything.
		b.report("a", protocol.PhaseDone, false, protocol.StepRefused)
		b.get("a").Status.Needs = nil
		b.step()

		r := b.mustRollout(packagesKey("2.0"))
		assert.Equal(t, fleet.HostDone, r.Host("a").State)
		assert.Equal(t, "b", r.Canary)
		assert.Equal(t, []string{"b"}, b.leasedHosts(), "one new canary, alone")

		// The hold is lifted: the old canary rejoins and waits for the new
		// one like every other host.
		b.wants("a", packagesNeed("2.0"))

		out := b.step()

		assert.Equal(t, []string{"b"}, b.leasedHosts())
		assert.Equal(t, "after canary b", queueReasons(out.Plan)["a"])
		assert.Equal(t, fleet.StageRest, b.mustRollout(packagesKey("2.0")).Host("a").Stage)
	})

	t.Run("a failed canary halts and nobody follows", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 100
		b.w.Settings.MinParticipationPercent = 10
		b.host("a", 5)
		b.host("b", 6)
		b.host("c", 7)
		b.host("down", 9)

		for _, h := range []string{"a", "b", "c"} {
			b.wants(h, packagesNeed("2.0"))
		}

		b.step()
		require.Equal(t, []string{"a"}, b.leasedHosts())
		b.report("a", protocol.PhaseFailed, true)

		out := b.step()
		key := packagesKey("2.0")

		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, fleet.RolloutHalted, b.mustRollout(key).State)
		assert.Equal(t, "a", b.mustRollout(key).Canary)
		assert.Equal(t, map[string]string{
			"b": "rollout halted: a: container did not start",
			"c": "rollout halted: a: container did not start",
		}, queueReasons(out.Plan))
		assert.Equal(t, SlotFailed, out.Plan.Hosts["a"].Slot)

		// It stays so, pass after pass, and an offline host that joins does not
		// move either.
		b.wants("down", packagesNeed("2.0"))
		b.liveOverride["down"] = 0
		b.advance(testEpoch)

		out = b.step()
		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, "rollout halted: a: container did not start", queueReasons(out.Plan)["down"])
		delete(b.liveOverride, "down")
		b.get("down").Status.Needs = nil
		b.advance(testEpoch)
		b.step()

		// A person reopens the rollout without retrying the canary. A failure
		// is not a success: one new canary goes alone, not everybody at once.
		b.mustRollout(key).State = fleet.RolloutActive
		b.mustRollout(key).Reason = ""

		out = b.step()
		assert.Equal(t, []string{"b"}, b.leasedHosts())
		assert.Equal(t, "b", b.mustRollout(key).Canary)
		assert.Equal(t, fleet.HostFailed, b.mustRollout(key).Host("a").State)
		assert.Equal(t, map[string]string{"c": "after canary b"}, queueReasons(out.Plan))

		b.serve("b")
		assert.Equal(t, []string{"c"}, b.leasedHosts())
	})
}

func TestHoldDirectives(t *testing.T) {
	b := newWorldBuilder(t)
	h := b.host("a", 0)

	pending := func() []protocol.DirectiveKind {
		var kinds []protocol.DirectiveKind

		for _, d := range b.w.Directives {
			if d.State == fleet.DirectivePending {
				kinds = append(kinds, d.Kind)
			}
		}

		return kinds
	}

	b.step()
	assert.Empty(t, pending(), "nothing to say while both sides agree")

	h.HoldWanted = true
	h.HoldReason = "debugging"
	b.step()
	b.step()
	require.Equal(t, []protocol.DirectiveKind{protocol.DirectiveHold}, pending(), "issued once")
	assert.Equal(t, "debugging", b.w.Directives[0].Reason)
	assert.Equal(t, "a", b.w.Directives[0].Host)

	// Wanted flips before the agent answered: the hold is withdrawn.
	h.HoldWanted = false
	b.step()
	assert.Empty(t, pending())
	assert.Equal(t, fleet.DirectiveCancelled, b.w.Directives[0].State)

	h.HoldWanted = true
	b.step()
	require.Equal(t, []protocol.DirectiveKind{protocol.DirectiveHold}, pending())

	// The agent agrees.
	h.Status.Hold.Controller = true
	b.step()
	assert.Empty(t, pending())
	assert.Equal(t, fleet.DirectiveAccepted, b.w.Directives[len(b.w.Directives)-1].State)

	// Unhold: release-hold until the agent reports the hold gone. A local
	// hold file is not the controller's business.
	h.HoldWanted = false
	h.Status.Hold.Local = true
	b.step()
	require.Equal(t, []protocol.DirectiveKind{protocol.DirectiveReleaseHold}, pending())

	h.Status.Hold.Controller = false
	b.step()
	assert.Empty(t, pending())
}

// A host that reported a controller hold when the controller first saw it
// (the database was lost) has its row created with the hold wanted
// (store.RecordSync). The scheduler then finds both sides in agreement: it
// neither lifts the hold nor grants the host anything.
func TestAdoptedControllerHoldIsNotReleased(t *testing.T) {
	b := newWorldBuilder(t)
	h := b.host("a", 0)
	h.HoldWanted = true
	h.HoldReason = store.HoldReasonAdopted
	h.Status.Hold.Controller = true
	b.host("b", 0)
	b.wants("b", packagesNeed("2.0"))

	for range 3 {
		out := b.step()
		assert.Empty(t, out.Directives, "no release-hold, no hold")
		assert.Equal(t, SlotHeld, out.Plan.Hosts["a"].Slot)
	}

	assert.Empty(t, b.w.Directives)
	assert.Equal(t, []string{"b"}, b.leasedHosts())

	// What the adoption prevents: the same report on a row without the hold
	// wanted is a hold to lift.
	h.HoldWanted = false
	h.HoldReason = ""

	out := b.step()
	require.Len(t, out.Directives, 1)
	assert.Equal(t, protocol.DirectiveReleaseHold, out.Directives[0].Kind)
	assert.Equal(t, "a", out.Directives[0].Host)
}

func TestStaggeredManualRolloutHasACanary(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.PolicyReboot = fleet.PolicyManual // manual rollouts answer to no policy
	b.host("v1", 5)
	b.host("v2", 6)
	b.host("tool", 0)
	b.manualRollout("m1", false, false, "v1", "v2", "tool")

	out := b.step()
	assert.Equal(t, []string{"v1"}, b.leasedHosts())
	assert.Equal(t, "v1", b.mustRollout("manual:m1").Canary)
	assert.Equal(t, map[string]string{"tool": "after canary v1", "v2": "after canary v1"}, queueReasons(out.Plan))
	assert.False(t, b.lease("v1").Force)

	b.serve("v1")
	assert.Equal(t, []string{"tool", "v2"}, b.leasedHosts())
}

func TestManualDirectives(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("yes", 0)
	b.host("no", 0)
	b.host("slow", 0)

	r := &fleet.Rollout{
		ID: "m1", Kind: fleet.RolloutManual, Key: "manual:m1", State: fleet.RolloutActive,
		Approved: true, CreatedBy: "alice", CreatedAt: b.w.Now,
	}

	for _, name := range []string{"yes", "no", "slow"} {
		r.Hosts = append(r.Hosts, fleet.RolloutHost{Host: name, State: fleet.HostExpected})
		b.w.Directives = append(b.w.Directives, &fleet.Directive{
			Directive: protocol.Directive{ID: "d-" + name, Kind: protocol.DirectiveReboot},
			Host:      name, State: fleet.DirectivePending, RolloutID: "m1", CreatedBy: "alice",
		})
	}

	b.w.Rollouts = []*fleet.Rollout{r}

	b.step()
	assert.Equal(t, map[string]fleet.HostState{
		"yes": fleet.HostExpected, "no": fleet.HostExpected, "slow": fleet.HostExpected,
	}, memberStates(b.mustRollout("manual:m1")), "nothing answered yet; the rollout stays open")

	b.get("yes").Status.DirectiveAcks = []protocol.DirectiveAck{{ID: "d-yes", Status: protocol.DirectiveAccepted}}
	b.wants("yes", protocol.Need{
		ID: protocol.ManualNeedID("d-yes"), Kind: protocol.NeedReboot, DirectiveID: "d-yes",
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	})
	b.get("no").Status.DirectiveAcks = []protocol.DirectiveAck{
		{ID: "d-no", Status: protocol.DirectiveRejected, Error: "verb reboot not allowed"},
	}

	b.step()

	m := b.mustRollout("manual:m1")
	assert.Equal(t, fleet.HostLeased, m.Host("yes").State)
	assert.Equal(t, fleet.HostSkipped, m.Host("no").State)
	assert.Equal(t, "verb reboot not allowed", m.Host("no").Error)
	assert.Equal(t, fleet.HostExpected, m.Host("slow").State)
	assert.Empty(t, b.rollout("reboot"), "a manual need opens no observed-cause rollout")

	states := map[string]fleet.DirectiveState{}
	for _, d := range b.w.Directives {
		states[d.ID] = d.State
	}

	assert.Equal(t, map[string]fleet.DirectiveState{
		"d-yes": fleet.DirectiveAccepted, "d-no": fleet.DirectiveRejected, "d-slow": fleet.DirectivePending,
	}, states)

	// The silent host stops blocking after hostStaleAfter; its directive is
	// cancelled when the rollout closes.
	b.serve("yes")
	b.step()

	closed := b.closed("manual:m1")
	require.NotNil(t, closed)
	assert.Equal(t, fleet.RolloutCompleted, closed.State)

	for _, d := range b.w.Directives {
		if d.ID == "d-slow" {
			assert.Equal(t, fleet.DirectiveCancelled, d.State)
		}
	}
}

// overrideNeed is the need of a host whose overrides file says something
// else than its container runs.
func overrideNeed(container, target string) protocol.Need {
	return protocol.Need{
		ID: protocol.OverrideNeedID(container, target), Kind: protocol.NeedOverride, Local: true,
		Override: &protocol.OverrideNeed{
			Container: container, Image: "ethpandaops/geth:fix", ToDigest: digestA,
		},
	}
}

// What a host asks for itself (its overrides file, a roll requested on the
// host) is one rollout of that host alone. It answers to no policy and has
// no canary, and it goes through the budget like everything else.
func TestLocalRollout(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyImage = fleet.PolicyManual
	b.host("a", 0)
	b.host("b", 0)

	local := imageNeed("beacon", "sigp/lighthouse:latest", digestA)
	local.ID, local.Local = protocol.LocalNeedID("1"), true

	b.wants("a", overrideNeed("geth", "one"), local)
	b.wants("b", overrideNeed("geth", "one"))
	out := b.step()

	key := localKey("a", b.get("a").Status.Needs)
	r := b.mustRollout(key)
	assert.Equal(t, fleet.RolloutLocal, r.Kind)
	assert.Equal(t, "a: override geth, roll beacon", r.Title)
	assert.Empty(t, r.Canary)
	assert.Equal(t, map[string]fleet.HostState{"a": fleet.HostLeased}, memberStates(r))
	assert.Nil(t, b.rollout(imageKey("", "sigp/lighthouse:latest", digestA)),
		"a roll requested on the host is no image rollout")

	l := b.lease("a")
	require.NotNil(t, l, "no approval is waited for")
	assert.Len(t, l.Needs, 2, "one lease serves both")
	assert.Nil(t, b.lease("b"), "the host limit is one of two")
	assert.Equal(t, "host limit 1 of 1", queueReasons(out.Plan)["b"])

	b.serve("a")
	assert.Equal(t, fleet.RolloutCompleted, b.closed(key).State)
	require.NotNil(t, b.lease("b"))
}

func TestLocalRolloutFollowsWhatTheHostAsksFor(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.Paused = true
	b.host("a", 0)
	b.wants("a", overrideNeed("geth", "one"))
	b.step()

	first := localKey("a", b.get("a").Status.Needs)
	require.NotNil(t, b.rollout(first))

	// The file changed again.
	b.get("a").Status.Needs = []protocol.Need{overrideNeed("geth", "two")}
	b.step()

	assert.Equal(t, fleet.RolloutSuperseded, b.closed(first).State)
	assert.Equal(t, "the host asks for something else now", b.closed(first).Reason)

	second := localKey("a", b.get("a").Status.Needs)
	require.NotNil(t, b.rollout(second))

	// The entry was taken out before it was applied.
	b.get("a").Status.Needs = nil
	b.step()

	assert.Equal(t, "the host no longer asks for it", b.closed(second).Reason)
}

// A lease that fails halts the host's rollout; it grants nothing again until
// a person retries it or the host asks for something else.
func TestLocalRolloutHaltsOnFailure(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.wants("a", overrideNeed("geth", "one"))
	b.step()

	key := localKey("a", b.get("a").Status.Needs)

	b.report("a", protocol.PhaseFailed, true)
	b.step()

	r := b.mustRollout(key)
	assert.Equal(t, fleet.RolloutHalted, r.State)
	assert.Empty(t, b.get("a").Broken, "what a host asks for itself belongs to no group")

	b.get("a").Status.Operation = nil
	b.step()
	assert.Nil(t, b.lease("a"))

	b.get("a").Status.Needs = []protocol.Need{overrideNeed("geth", "two")}
	b.step()

	assert.Equal(t, fleet.RolloutSuperseded, b.closed(key).State)
	require.NotNil(t, b.lease("a"), "what the host asks for now is served")
}

// One image followed by the containers of several clients opens a rollout per
// client, each with the client's hosts and under the client's image policy.
func TestImageRolloutPerClient(t *testing.T) {
	const image = "ethpandaops/dummy:devnet"

	clients := []struct {
		client  string
		policy  fleet.Policy
		granted bool
	}{
		{client: "teku", policy: fleet.PolicyAuto, granted: true},
		{client: "lighthouse", policy: fleet.PolicyManual, granted: false},
		{client: "lodestar", policy: fleet.PolicyAuto, granted: true},
	}

	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 50 // room for a canary per client
	b.w.Settings.PolicyClients = make(fleet.ClientPolicies, len(clients))

	for _, tt := range clients {
		b.w.Settings.PolicyClients[tt.client] = tt.policy

		for _, name := range []string{tt.client + "-1", tt.client + "-2"} {
			c := container("cl", image, digestOld, digestA, b.w.Now)
			c.Client = tt.client
			n := imageNeed("cl", image, digestA)
			n.Image.Client = tt.client

			h := b.host(name, 0)
			h.Status.Containers = []protocol.Container{c}
			h.Status.Needs = []protocol.Need{n}
		}
	}

	out := b.step()
	require.Len(t, b.w.Rollouts, len(clients))

	for _, tt := range clients {
		t.Run(tt.client, func(t *testing.T) {
			r := b.mustRollout(imageKey(tt.client, image, digestA))
			assert.Equal(t, tt.client, r.Client)
			members := slices.Collect(maps.Keys(memberStates(r)))
			assert.ElementsMatch(t, []string{tt.client + "-1", tt.client + "-2"}, members)

			leased := 0

			for _, m := range r.Hosts {
				if l := b.lease(m.Host); l != nil {
					assert.Contains(t, slices.Collect(maps.Values(l.Rollouts)), r.ID)
					leased++
				} else if !tt.granted {
					assert.Equal(t, "waiting for approval (policy manual)", queueReasons(out.Plan)[m.Host])
				}
			}

			assert.Equal(t, tt.granted, leased > 0, "a lease for the canary")
		})
	}
}
