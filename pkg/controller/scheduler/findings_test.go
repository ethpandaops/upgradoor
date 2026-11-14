package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The tests in this file replay the scenarios of the first review of the
// canary ordering, one per finding, under the rules as they are now. Finding
// 4 (readiness while liveness is unknown) is
// TestReview1Finding4ValidatorHostIsNotReadyWhileLivenessIsUnknown in
// ready_test.go. The findings of the second review are in findings2_test.go.

// failedOnDigestA builds a fleet in which the host failing rolled to digest A
// and failed, so that the rollout of digest A is halted with it.
func failedOnDigestA(t *testing.T, failing string, hosts map[string]uint64) *worldBuilder {
	t.Helper()

	b := newWorldBuilder(t)

	for _, name := range sortedKeys(hosts) {
		b.host(name, hosts[name])
		b.wantsImage(name, imgLighthouse, digestA)
		b.get(name).HoldWanted = name != failing
	}

	b.step()
	require.Equal(t, []string{failing}, b.leasedHosts())
	b.report(failing, protocol.PhaseFailed, true)

	for name := range hosts {
		b.get(name).HoldWanted = false
	}

	b.step()
	require.Equal(t, fleet.RolloutHalted, b.mustRollout(imageKey(imgLighthouse, digestA)).State)
	require.Empty(t, b.leasedHosts())

	return b
}

// seesDigestB makes the hosts report the tag at digest B.
func seesDigestB(b *worldBuilder, hosts ...string) {
	for _, name := range hosts {
		h := b.get(name)
		h.Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestB, b.w.Now)}
		h.Status.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digestB)}
		h.Status.Operation = nil
	}
}

// Finding 1: a host carried over as failed still attests and waits for the
// gate, which a second host closed by going down. The host that is down is
// repaired through the closed gate, whatever the order of the rollout, once
// it has been offline for two epochs; the gate reopens and everybody follows.
// Before, the carried host was the canary, waited for the gate, and the host
// that is down waited for the canary: nothing ever moved.
func TestReview1Finding1DownHostIsRepairedWhileACarriedHostWaitsForTheGate(t *testing.T) {
	b := failedOnDigestA(t, "a", map[string]uint64{"a": 32, "b": 32, "c": 32, "d": 32})

	// "d" goes down completely: 96 of 128 attest, the gate closes.
	b.advance(testEpoch)
	b.liveOverride["d"] = 0
	b.step()

	// The tag moves to digest B. One epoch without attestations is not
	// enough to pass the closed gate.
	b.advance(time.Minute)
	seesDigestB(b, "a", "b", "c", "d")

	out := b.step()
	key := imageKey(imgLighthouse, digestB)

	require.False(t, out.Plan.Gate.OK)
	require.Empty(t, b.leasedHosts())
	assert.Equal(t, "network gate: participation 75% < 90%", queueReasons(out.Plan)["d"])

	// A second one is.
	b.advance(testEpoch)

	out = b.step()
	r := b.mustRollout(key)

	require.False(t, out.Plan.Gate.OK)
	assert.Equal(t, []string{"participation 75% < 90%"}, out.Plan.Gate.Reasons)
	assert.Equal(t, []string{"d"}, b.leasedHosts(), "the host that is down is repaired through the closed gate")
	assert.True(t, b.lease("d").Repair)
	assert.Equal(t, fleet.StageRepair, r.Host("a").Stage)
	assert.Equal(t, fleet.StageRest, r.Host("d").Stage, "an ordinary member")
	assert.Empty(t, r.Canary)
	assert.Equal(t, map[string]string{
		"a": "network gate: participation 75% < 90%",
		"b": "network gate: participation 75% < 90%",
		"c": "network gate: participation 75% < 90%",
	}, queueReasons(out.Plan))

	// "d" comes back: the gate reopens and the carried host is next, alone.
	delete(b.liveOverride, "d")
	b.serve("d")

	out = b.step()
	assert.True(t, out.Plan.Gate.OK)
	assert.Equal(t, []string{"a"}, b.leasedHosts())
	assert.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(out.Plan))

	for _, name := range []string{"a", "b", "c"} {
		require.Equal(t, []string{name}, b.leasedHosts())
		b.serve(name)
	}

	assert.Equal(t, fleet.RolloutCompleted, b.closed(key).State)
}

// Finding 2: the host that failed in the superseded rollout has no
// validators. It is repaired first all the same.
func TestReview1Finding2CarriedHostWithoutValidatorsIsRepairedFirst(t *testing.T) {
	key := imageKey(imgLighthouse, digestB)

	// Case 1: all three hosts report the new digest. Before, "h1" was the
	// canary and "h3" waited for it.
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
	// Before, "h1" was leased at once as the canary.
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

// Finding 3: a host that cannot fit the budget is not the canary while
// another host fits. Before, the canary was a host with validators whatever
// the budget, and the hosts that cost nothing waited for it for ever.
func TestReview1Finding3CanaryThatCannotFitTheBudgetDoesNotStallTheRollout(t *testing.T) {
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
