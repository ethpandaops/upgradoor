package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// What the end of a lease does to the marks of its host: a failed lease
// marks the host for every group it carried, except a group whose steps were
// done while another step failed; a released lease clears the groups whose
// steps were done; an expired lease changes nothing.
func TestLeaseEndSetsAndClearsTheBrokenMarks(t *testing.T) {
	const (
		image    = prefixImageKey + imgLighthouse
		packages = groupPackages
		old      = "old"
	)

	tests := []struct {
		name string
		// before are the host's marks; report is what the agent says about
		// the image step and the packages step of its lease.
		before  map[string]string
		phase   protocol.Phase
		steps   []protocol.StepStatus
		silent  bool
		want    []string
		cleared []string
	}{
		{
			name:  "a failed step breaks its group, a step that was not begun too",
			phase: protocol.PhaseFailed,
			steps: []protocol.StepStatus{protocol.StepFailed, protocol.StepPending},
			want:  []string{image, packages},
		},
		{
			name:  "a group that was done is spared when another step failed",
			phase: protocol.PhaseFailed,
			steps: []protocol.StepStatus{protocol.StepDone, protocol.StepFailed},
			want:  []string{packages},
		},
		{
			name:   "an agent that goes silent breaks everything the lease carried",
			phase:  protocol.PhaseApplying,
			steps:  []protocol.StepStatus{protocol.StepDone, protocol.StepRunning},
			silent: true,
			want:   []string{image, packages},
		},
		{
			name:    "a released lease clears what it did",
			before:  map[string]string{image: old, packages: old},
			phase:   protocol.PhaseDone,
			steps:   []protocol.StepStatus{protocol.StepDone, protocol.StepDone},
			cleared: []string{image, packages},
		},
		{
			name:    "a released lease leaves the group whose step it did not do",
			before:  map[string]string{image: old, packages: old},
			phase:   protocol.PhaseDone,
			steps:   []protocol.StepStatus{protocol.StepDone, protocol.StepSuperseded},
			cleared: []string{image},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.host("a", 10)
			b.host("z", 90)
			b.get("a").Broken = tt.before
			b.get("a").Status.Containers = []protocol.Container{
				container("lighthouse", imgLighthouse, digestOld, digestA, b.w.Now),
			}
			b.wants("a", imageNeed("lighthouse", imgLighthouse, digestA), packagesNeed("2.0"), rebootNeed())
			b.step()

			lease := b.lease("a")
			require.NotNil(t, lease)
			require.Len(t, lease.Needs, 3, "image, packages and a reboot, which belongs to no group")

			// Nothing new is granted while the test looks at the end of the lease,
			// and what the agent did is no longer a need.
			b.w.Settings.Paused = true
			statuses := append(tt.steps, protocol.StepDone)
			b.report("a", tt.phase, true, statuses...)

			for i, n := range lease.Needs {
				if statuses[i] == protocol.StepDone {
					b.get("a").Status.Needs = slices.DeleteFunc(b.get("a").Status.Needs,
						func(r protocol.Need) bool { return r.ID == n.ID })
				}
			}

			if tt.silent {
				b.step()
				b.advance(31*time.Minute, "a")
				b.step()
			} else {
				b.step()
				b.advance(5 * time.Second)
				b.step()
				b.advance(2 * testEpoch)
				b.step()
			}

			require.Nil(t, b.lease("a"))
			require.Equal(t, lease.ID, b.lastLease("a").ID)

			want := make(map[string]string, 2)
			for group, id := range tt.before {
				want[group] = id
			}

			for _, group := range tt.want {
				want[group] = lease.ID
			}

			for _, group := range tt.cleared {
				delete(want, group)
			}

			assert.Equal(t, want, nonNilMarks(b.get("a").Broken))
		})
	}

	t.Run("a lease that expires says nothing", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 0)
		b.get("a").Broken = map[string]string{packages: old}
		b.wants("a", packagesNeed("2.0"))
		b.step()

		b.report("a", protocol.PhaseDone, false, protocol.StepSuperseded)

		out := b.step()
		require.Equal(t, fleet.LeaseExpired, out.Leases[0].State)
		assert.Empty(t, out.Broken)
		assert.Equal(t, map[string]string{packages: old}, b.get("a").Broken)
	})
}

func nonNilMarks(marks map[string]string) map[string]string {
	if marks == nil {
		return map[string]string{}
	}

	return marks
}

// A host that is broken for the group of a rollout goes first in it, before
// any canary, and the others wait for it; a rollout of another group does not
// know the mark. Repaired, the host is a member like any other, and its
// repair is the rollout's first success.
func TestBrokenHostGoesFirst(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100

	for _, name := range []string{"a", "b", "c"} {
		b.host(name, 10)
		b.wants(name, packagesNeed("2.0"))
	}

	b.broke("c", packagesNeed("1.5"))
	b.broke("b", imageNeed("lighthouse", imgLighthouse, digestA))

	out := b.step()
	r := b.mustRollout(packagesKey("2.0"))

	assert.Equal(t, []string{"c"}, b.leasedHosts())
	assert.Empty(t, r.Canary, "no canary while a broken host is still to be served")
	assert.Equal(t, fleet.StageRepair, r.Host("c").Stage)
	assert.Equal(t, fleet.StageRest, r.Host("b").Stage, "broken for an image, not for the packages")
	assert.Equal(t, map[string]string{"a": "after c", "b": "after c"}, queueReasons(out.Plan))

	b.serve("c")
	r = b.mustRollout(packagesKey("2.0"))

	assert.Empty(t, b.get("c").Broken)
	assert.Equal(t, fleet.StageRepair, r.Host("c").Stage, "the stage says how it was served")
	assert.Empty(t, r.Canary, "the repair was the first success")
	assert.Equal(t, []string{"a", "b"}, b.leasedHosts())
	assert.Equal(t, map[string]string{prefixImageKey + imgLighthouse: "broke-b"}, b.get("b").Broken)
}

// The host that failed in the superseded rollout has no validators. It is
// repaired first all the same.
func TestBrokenHostWithoutValidatorsGoesFirst(t *testing.T) {
	key := imageKey(imgLighthouse, digestB)

	// Case 1: all three hosts report the new digest.
	t.Run("everybody reports", func(t *testing.T) {
		b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 0})
		b.advance(time.Minute)
		seesDigestB(b, "h1", "h2", "h3")

		out := b.step()
		r := b.mustRollout(key)

		assert.Equal(t, []string{"h3"}, b.leasedHosts())
		assert.Equal(t, fleet.StageRepair, r.Host("h3").Stage)
		assert.Empty(t, r.Canary)
		assert.Equal(t, map[string]string{"h1": "after h3", "h2": "after h3"}, queueReasons(out.Plan))

		// Its repair is the first success: the others follow without a canary.
		b.serve("h3")
		assert.Equal(t, []string{"h1"}, b.leasedHosts())

		out = b.step()
		assert.Equal(t, map[string]string{"h2": "budget 50% of 10%"}, queueReasons(out.Plan))
	})

	// Case 2: only "h1" has seen the new digest, "h3" is still expected to.
	t.Run("the carried host is still expected", func(t *testing.T) {
		b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 0})
		b.advance(time.Minute)
		seesDigestB(b, "h1")

		out := b.step()
		r := b.mustRollout(key)

		assert.Empty(t, b.leasedHosts(), "nobody before the host carried over had its turn")
		assert.Equal(t, fleet.HostExpected, r.Host("h3").State)
		assert.Equal(t, fleet.StageRepair, r.Host("h3").Stage)
		assert.Equal(t, map[string]string{"h1": "after h3"}, queueReasons(out.Plan))

		// It reports: it goes first.
		b.advance(10 * time.Second)
		seesDigestB(b, "h3")

		out = b.step()
		assert.Equal(t, []string{"h3"}, b.leasedHosts())
		assert.Equal(t, map[string]string{"h1": "after h3"}, queueReasons(out.Plan))
	})

	// A carried host that never reports is waited for hostStaleAfter, not for
	// ever.
	t.Run("the carried host never reports", func(t *testing.T) {
		b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 0})
		b.advance(time.Minute)
		seesDigestB(b, "h1")
		b.step()
		require.Empty(t, b.leasedHosts())

		b.advance(b.w.Settings.HostStaleAfter.Std() + time.Second)
		b.step()
		assert.Equal(t, []string{"h1"}, b.leasedHosts())
	})
}

// A broken host stays first through pending, expected and pending again: the
// mark is the host's, whatever the member's state.
func TestBrokenHostStaysFirstThroughStateChanges(t *testing.T) {
	b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 10})
	require.Equal(t, map[string]string{prefixImageKey + imgLighthouse: b.lastLease("h3").ID}, b.get("h3").Broken)
	b.advance(time.Minute)
	seesDigestB(b, "h1", "h2", "h3")

	// Nothing is granted meanwhile.
	b.w.Settings.Paused = true
	b.step()

	key := imageKey(imgLighthouse, digestB)
	require.Equal(t, fleet.StageRepair, b.mustRollout(key).Host("h3").Stage)
	require.Equal(t, fleet.HostPending, b.mustRollout(key).Host("h3").State)

	// The host goes on hold: a held agent reports no need.
	needs := b.get("h3").Status.Needs
	b.get("h3").HoldWanted = true
	b.get("h3").Status.Needs = nil
	b.step()
	require.Equal(t, fleet.HostExpected, b.mustRollout(key).Host("h3").State)
	assert.Equal(t, fleet.StageRepair, b.mustRollout(key).Host("h3").Stage)

	// The hold is lifted: it is still the host that goes first.
	b.get("h3").HoldWanted = false
	b.get("h3").Status.Needs = needs
	b.w.Settings.Paused = false

	out := b.step()
	r := b.mustRollout(key)

	assert.Equal(t, []string{"h3"}, b.leasedHosts())
	assert.Equal(t, fleet.StageRepair, r.Host("h3").Stage)
	assert.Empty(t, r.Canary)
	assert.Equal(t, map[string]string{"h1": "after h3", "h2": "after h3"}, queueReasons(out.Plan))

	// Served, it is in order: the mark is gone, and the stage says how it was
	// served. Its repair was the first success, so nobody waits for a canary.
	b.serve("h3")
	r = b.mustRollout(key)
	assert.Equal(t, fleet.HostDone, r.Host("h3").State)
	assert.Empty(t, r.Host("h3").Error)
	assert.Equal(t, fleet.StageRepair, r.Host("h3").Stage)
	assert.Empty(t, b.get("h3").Broken)
	assert.Equal(t, []string{"h1"}, b.leasedHosts())
}

// H3 failed in A, goes first in B and holds its lease there when the tag moves
// again. In C it is still the host that goes first.
func TestBrokenHostStaysFirstThroughAChainOfSupersedes(t *testing.T) {
	b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 10})
	b.w.Settings.MaxUnavailablePercent = 100

	b.advance(time.Minute)
	seesDigestB(b, "h1", "h2", "h3")
	b.step()

	second := b.mustRollout(imageKey(imgLighthouse, digestB))
	require.Equal(t, []string{"h3"}, b.leasedHosts())
	require.Equal(t, fleet.StageRepair, second.Host("h3").Stage)

	b.report("h3", protocol.PhaseApplying, true)
	b.step()

	// The tag moves again while h3 is out.
	b.advance(time.Minute)
	seesDigest(b, digestC, "h1", "h2")
	b.report("h3", protocol.PhaseApplying, true)

	out := b.step()
	third := b.mustRollout(imageKey(imgLighthouse, digestC))

	assert.Equal(t, []string{"h3"}, b.leasedHosts())
	assert.Equal(t, map[string]string{"h1": "after h3", "h2": "after h3"}, queueReasons(out.Plan))

	// Its lease of B fails: in C it goes first, and nobody else is granted.
	b.advance(time.Minute)
	b.report("h3", protocol.PhaseFailed, true)

	out = b.step()
	third = b.mustRollout(third.Key)

	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, fleet.StageRepair, third.Host("h3").Stage)
	assert.Empty(t, third.Canary)
	assert.Equal(t, map[string]string{"h1": "after h3", "h2": "after h3"}, queueReasons(out.Plan))

	b.advance(time.Minute)
	seesDigest(b, digestC, "h3")
	b.step()
	assert.Equal(t, []string{"h3"}, b.leasedHosts())

	// A broken host that still waits when the tag moves goes first as well.
	t.Run("a broken host that waits", func(t *testing.T) {
		b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 10})

		b.w.Settings.Paused = true
		b.advance(time.Minute)
		seesDigestB(b, "h1", "h2", "h3")
		b.step()
		b.advance(time.Minute)
		seesDigest(b, digestC, "h1", "h2", "h3")
		b.w.Settings.Paused = false
		b.step()

		third := b.mustRollout(imageKey(imgLighthouse, digestC))
		assert.Equal(t, []string{"h3"}, b.leasedHosts())
		assert.Equal(t, fleet.StageRepair, third.Host("h3").Stage)
	})
}

// A host that failed in the superseded rollout and is no member of the successor
// when that is created (its container runs a custom image for a while) goes
// first when it joins.
func TestBrokenHostThatJoinsTheNextRolloutLateGoesFirst(t *testing.T) {
	b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 10})
	key := imageKey(imgLighthouse, digestB)

	// Somebody swaps h3's image by hand: it is no host of the image now.
	b.get("h3").Status.Containers[0].Image = "someone/debug:build"
	b.get("h3").Status.Needs = nil
	b.get("h3").Status.Operation = nil

	b.w.Settings.Paused = true
	b.advance(time.Minute)
	seesDigestB(b, "h1", "h2")
	b.step()

	r := b.mustRollout(key)
	require.Nil(t, r.Host("h3"), "no member")

	// The configured image is back: h3 reports the need and joins.
	b.w.Settings.Paused = false
	b.advance(time.Minute)
	seesDigestB(b, "h3")

	out := b.step()
	r = b.mustRollout(key)

	assert.Equal(t, fleet.StageRepair, r.Host("h3").Stage)
	assert.Equal(t, fleet.StageRepair, r.Host("h3").Stage)
	assert.Equal(t, []string{"h3"}, b.leasedHosts())
	assert.Equal(t, map[string]string{"h1": "after h3", "h2": "after h3"}, queueReasons(out.Plan))
}
