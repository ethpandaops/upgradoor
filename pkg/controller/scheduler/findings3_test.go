package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The tests in this file replay the scenarios of the third review of the
// scheduler, one per finding.

const digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// seesDigest makes the hosts report the tag at a digest while they still run
// the old one.
func seesDigest(b *worldBuilder, digest string, hosts ...string) {
	for _, name := range hosts {
		h := b.get(name)
		h.Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digest, b.w.Now)}
		h.Status.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digest)}
		h.Status.Operation = nil
	}
}

// Finding 1, first scenario: a repair lease does not open the gate. Default
// settings, 18 validator hosts and 4 without; two validator hosts have been
// down for two epochs (88.8%). Before, the gate left every leased host out of
// the participation: with the repair granted the gate read as open and a
// bootnode was leased as the canary while real participation was 88.8%.
func TestReview3Finding1RepairLeaseDoesNotOpenTheGate(t *testing.T) {
	b := newWorldBuilder(t)

	for i := range 18 {
		b.host(fmt.Sprintf("val%02d", i), 10)
	}

	for i := range 4 {
		b.host(fmt.Sprintf("boot%d", i), 0)
	}

	for _, h := range b.w.Hosts {
		b.wants(h.Name, packagesNeed("2.0"))
	}

	b.down("val03")
	b.down("val07")

	out := b.step()
	require.Equal(t, []string{"participation 88.8% < 90%"}, out.Plan.Gate.Reasons)
	require.Equal(t, []string{"val03"}, b.leasedHosts(), "one repair")
	require.True(t, b.lease("val03").Repair)

	for range 3 {
		b.advance(10 * time.Second)
		b.report("val03", protocol.PhaseApplying, true)

		out = b.step()
		assert.Equal(t, []string{"participation 88.8% < 90%"}, out.Plan.Gate.Reasons,
			"the validators that were offline before the lease still count")
		assert.Equal(t, []string{"val03"}, b.leasedHosts(), "nobody else gets through")
	}

	// The host that is being repaired does not become the canary: its
	// return proves less than a canary must.
	assert.NotEqual(t, "val03", b.mustRollout(packagesKey("2.0")).Canary)

	reasons := queueReasons(out.Plan)
	assert.Equal(t, "network gate: participation 88.8% < 90%", reasons["boot0"])
	assert.Equal(t, "network gate: participation 88.8% < 90%", reasons["val00"])

	// The other offline host would pass the gate too, but one at a time.
	assert.Equal(t, "after repair of val03", reasons["val07"])

	for _, q := range out.Plan.Queue {
		if q.Host == "val07" {
			assert.Equal(t, WaitOrder, q.Wait)
			assert.Equal(t, "val03", q.After)
		}
	}
}

// Finding 1, second scenario: a host that missed one epoch is not let through
// next to an open repair. Before, with the repaired host out of the gate's
// sum, the gate read as open and `small` went as an ordinary member that
// costs nothing.
func TestReview3Finding1NoOrdinaryGrantNextToARepair(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("big", 30)
	b.host("small", 5)
	b.host("rest", 65)

	for _, h := range b.w.Hosts {
		b.wants(h.Name, packagesNeed("2.0"))
	}

	b.down("big")
	b.fillLiveness()
	b.live(b.w.Health.LivenessEpoch, "small", 0)

	out := b.step()
	require.False(t, out.Plan.Gate.OK)
	require.Equal(t, []string{"big"}, b.leasedHosts())
	require.True(t, b.lease("big").Repair)

	b.advance(10 * time.Second)
	b.report("big", protocol.PhaseApplying, true)

	out = b.step()
	assert.Equal(t, []string{"big"}, b.leasedHosts())
	assert.Equal(t, []string{"participation 65% < 90%"}, out.Plan.Gate.Reasons)
	assert.Equal(t, "network gate: participation 65% < 90%", queueReasons(out.Plan)["small"])
}

// Finding 1, third scenario: with a looser budget (30%) and gate (85%), a
// healthy host is not leased next to the repair. Before, real participation
// fell to 70%.
func TestReview3Finding1NoHealthyGrantNextToARepair(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 30
	b.w.Settings.MinParticipationPercent = 85

	for i := range 10 {
		name := fmt.Sprintf("h%d", i)
		b.host(name, 10)
		b.wants(name, packagesNeed("2.0"))
	}

	b.down("h4")
	b.down("h8")

	out := b.step()
	require.Equal(t, []string{"participation 80% < 85%"}, out.Plan.Gate.Reasons)
	require.Equal(t, []string{"h4"}, b.leasedHosts())

	b.advance(10 * time.Second)
	b.report("h4", protocol.PhaseApplying, true)

	out = b.step()
	assert.Equal(t, []string{"h4"}, b.leasedHosts(), "8 of 10 attest: nobody healthy goes down as well")
	assert.Equal(t, []string{"participation 80% < 85%"}, out.Plan.Gate.Reasons)

	// A lease of a healthy host still does not close the gate on its rollout:
	// what it took offline is left out.
	t.Run("an ordinary lease takes its own validators out", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 30

		for i := range 10 {
			name := fmt.Sprintf("h%d", i)
			b.host(name, 10)
			b.wants(name, packagesNeed("2.0"))
		}

		b.step()
		require.Equal(t, []string{"h0"}, b.leasedHosts())
		assert.Equal(t, uint64(10), b.lease("h0").Charged)

		b.advance(testEpoch)
		b.liveOverride["h0"] = 0
		b.report("h0", protocol.PhaseApplying, true)

		out := b.step()
		assert.True(t, out.Plan.Gate.OK, "90 of the 90 others attest")
	})
}

// review3Running builds three validator hosts in the rollout of digest A, of
// which the canary "a" is in the middle of its operation.
func review3Running(t *testing.T) *worldBuilder {
	t.Helper()

	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100

	for _, name := range []string{"a", "b", "c"} {
		b.host(name, 10)
		b.wantsImage(name, imgLighthouse, digestA)
	}

	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())
	b.report("a", protocol.PhaseApplying, true)
	b.step()

	return b
}

// Finding 2: a lease that fails after its rollout was superseded makes the
// host broken: it goes first in the successor. While the lease is open the
// successor grants nobody else: it is its canary in flight. Before, the
// failure halted nothing, the host did not go first, the successor leased a
// canary in the same pass, and the member stayed `leased` in the closed
// rollout for ever.
func TestReview3Finding2LeaseOfASupersededRolloutThatFails(t *testing.T) {
	b := review3Running(t)
	keyA, keyB := imageKey(imgLighthouse, digestA), imageKey(imgLighthouse, digestB)
	first := b.mustRollout(keyA).ID
	lease := b.lease("a").ID

	// The tag moves while "a" is out. "b" and "c" see it.
	b.advance(time.Minute)
	seesDigest(b, digestB, "b", "c")
	b.report("a", protocol.PhaseApplying, true)

	out := b.step()
	r := b.mustRollout(keyB)

	require.Equal(t, fleet.RolloutSuperseded, b.closed(keyA).State)
	assert.Equal(t, []string{"a"}, b.leasedHosts(), "no second host down next to the lease of the old rollout")
	assert.Empty(t, r.Canary)
	assert.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(out.Plan))
	assert.Equal(t, fleet.StageRest, r.Host("a").Stage, "nobody knows yet how the lease ends")
	assert.Empty(t, r.Host("a").Error)

	// The lease fails. Nothing is halted, and nobody is granted in that pass.
	b.advance(time.Minute)
	b.report("a", protocol.PhaseFailed, true)

	out = b.step()
	r = b.mustRollout(keyB)

	assert.Equal(t, fleet.RolloutActive, r.State, "the successor is the fix")
	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, brokenBy(lease, first), r.Host("a").Error)
	assert.Equal(t, fleet.StageRepair, r.Host("a").Stage)
	assert.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(out.Plan))

	// The member of the closed rollout has the lease's outcome.
	old := b.closed(keyA)
	assert.Equal(t, fleet.HostFailed, old.Host("a").State)
	assert.Equal(t, "container did not start", old.Host("a").Error)
	assert.Equal(t, fleet.RolloutSuperseded, old.State)

	// "a" sees the new digest: it goes first.
	b.advance(time.Minute)
	seesDigest(b, digestB, "a")
	b.step()
	assert.Equal(t, []string{"a"}, b.leasedHosts())

	b.serve("a")
	assert.Equal(t, []string{"b", "c"}, b.leasedHosts(), "its repair was the first success")
}

// Finding 2, the other outcome: the lease of the superseded rollout is
// released ready. The host is simply in order: nobody goes first, and the
// successor starts with a canary as usual.
func TestReview3Finding2LeaseOfASupersededRolloutThatSucceeds(t *testing.T) {
	b := review3Running(t)
	keyA, keyB := imageKey(imgLighthouse, digestA), imageKey(imgLighthouse, digestB)

	b.advance(time.Minute)
	seesDigest(b, digestB, "b", "c")
	b.report("a", protocol.PhaseApplying, true)
	out := b.step()
	require.Equal(t, map[string]string{"b": "after a", "c": "after a"}, queueReasons(out.Plan))

	// "a" finishes on digest A, which is no longer the tag: it reports the
	// need for B right away.
	b.report("a", protocol.PhaseDone, true)
	seesDigestRunning(b, "a", digestA, digestB)
	b.step()
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)

	out = b.step()
	r := b.mustRollout(keyB)

	require.Contains(t, auditActions(out), "lease.released")
	assert.NotEqual(t, fleet.StageRepair, r.Host("a").Stage, "released: the host is in order")
	assert.Empty(t, r.Host("a").Error)
	assert.Equal(t, fleet.StageCanary, r.Host(r.Canary).Stage)
	assert.Len(t, b.leasedHosts(), 1, "one canary")
	assert.Equal(t, fleet.HostDone, b.closed(keyA).Host("a").State)
}

// seesDigestRunning makes the host run one digest and report the need for
// another.
func seesDigestRunning(b *worldBuilder, name, running, wanted string) {
	h := b.get(name)
	h.Status.Containers = []protocol.Container{container("c", imgLighthouse, running, wanted, b.w.Now)}
	h.Status.Needs = []protocol.Need{{
		ID:   protocol.ImageNeedID("c", wanted),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: "c", Image: imgLighthouse, FromDigest: running, ToDigest: wanted,
		},
	}}
}

// Finding 3, the chain: h3 failed in A, goes first in B and holds its lease
// there when the tag moves again. In C it is still the host that goes first.
// Before, the mark was only passed on for members that were waiting: in C h3
// was an ordinary member and a canary was granted elsewhere.
func TestReview3Finding3BrokenHostStaysFirstThroughAChainOfSupersedes(t *testing.T) {
	b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 10})
	b.w.Settings.MaxUnavailablePercent = 100
	first := b.mustRollout(imageKey(imgLighthouse, digestA)).ID

	b.advance(time.Minute)
	seesDigestB(b, "h1", "h2", "h3")
	b.step()

	second := b.mustRollout(imageKey(imgLighthouse, digestB))
	require.Equal(t, []string{"h3"}, b.leasedHosts())
	require.Equal(t, brokenBy(b.lastLease("h3").ID, first), second.Host("h3").Error)

	lease := b.lease("h3").ID
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
	assert.Equal(t, brokenBy(lease, second.ID), third.Host("h3").Error, "the lease in which it failed last")
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
		first := b.mustRollout(imageKey(imgLighthouse, digestA)).ID

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
		assert.Equal(t, brokenBy(b.lastLease("h3").ID, first), third.Host("h3").Error)
	})
}

// Finding 3, the late joiner: a host that failed in the superseded rollout
// and is no member of the successor when that is created (its container runs
// a custom image for a while) goes first when it joins. Before, the mark was
// given at creation only.
func TestReview3Finding3FailedHostThatJoinsTheSuccessorLate(t *testing.T) {
	b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 10})
	first := b.mustRollout(imageKey(imgLighthouse, digestA)).ID
	key := imageKey(imgLighthouse, digestB)

	// Somebody swaps h3's image by hand: it is no host of the image now.
	b.get("h3").Status.Containers[0].Custom = true
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

	assert.Equal(t, brokenBy(b.lastLease("h3").ID, first), r.Host("h3").Error)
	assert.Equal(t, fleet.StageRepair, r.Host("h3").Stage)
	assert.Equal(t, []string{"h3"}, b.leasedHosts())
	assert.Equal(t, map[string]string{"h1": "after h3", "h2": "after h3"}, queueReasons(out.Plan))
}

// forked gives the builder's chain a fork at an epoch that changes the slot
// duration, and keeps the beacon view's epochs in line with the clock.
func forked(b *worldBuilder, at uint64, slot time.Duration) {
	base := testSecondsPerSlot * time.Second
	phases := []fleet.SlotPhase{
		{Epoch: 0, Start: testGenesis, SlotMs: uint64(base / time.Millisecond)},
		{Epoch: at, Start: testGenesis.Add(time.Duration(at) * testEpoch), SlotMs: uint64(slot / time.Millisecond)},
	}

	b.healthMod = func(n *fleet.NetworkHealth) {
		n.Phases = phases
		n.SecondsPerSlot = uint64(slot / time.Second)

		current, _ := n.EpochAt(b.w.Now)
		n.CurrentEpoch = current
		n.FinalizedEpoch = current - 2
		n.LivenessEpoch = current - 1
	}
	b.refreshHealth()
}

// Finding 4: epochs are placed in time by the chain's slot phases, not by
// genesis plus the current slot duration. Before, a fork that halved the slot
// duration put every epoch days too early: every epoch "began before the
// operation was done" and no host with validators was ever ready. A fork that
// lengthened the slot erred the other way and released on an epoch from
// before the operation.
func TestReview3Finding4EpochTimesAcrossASlotDurationFork(t *testing.T) {
	t.Run("the slot duration halves", func(t *testing.T) {
		// The fork is at epoch 99; the clock is 394 s past it, in epoch 101
		// of 192 s epochs.
		b := newWorldBuilder(t)
		forked(b, 99, 6*time.Second)
		require.Equal(t, uint64(101), b.w.Health.CurrentEpoch)
		require.Equal(t, 192*time.Second, b.w.Health.EpochDuration())

		b.host("a", 10).Status.Node = nodeWith(8, 8)
		b.host("idle", 90)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		b.finish("a")
		b.step()
		b.advance(5 * time.Second)
		b.step()
		assert.Equal(t, "node at head with peers, waiting for it to stay so for 3m12s", b.lease("a").Readiness,
			"one epoch as it is now")

		// Epoch 101 ends: it began before the operation was done.
		b.advance(192 * time.Second)
		b.step()
		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

		// Epoch 102 began after it and is complete: ready.
		b.advance(192 * time.Second)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "ready: node at head with peers, validators attesting", releasedReadiness(t, out))
	})

	t.Run("the slot duration doubles", func(t *testing.T) {
		// The fork is at epoch 99; the clock is 394 s past it, still in epoch
		// 99 of 768 s epochs.
		b := newWorldBuilder(t)
		forked(b, 99, 24*time.Second)
		require.Equal(t, uint64(99), b.w.Health.CurrentEpoch)

		b.host("a", 10).Status.Node = nodeWith(8, 8)
		b.host("idle", 90)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		b.finish("a")
		b.step()
		b.advance(5 * time.Second)
		b.step()

		// Epoch 99 ends, full of attestations, but it began before the
		// operation was done.
		b.advance(768 * time.Second)
		b.step()
		require.NotNil(t, b.lease("a"), "an epoch from before the operation proves nothing")
		assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

		b.advance(768 * time.Second)
		b.step()
		assert.Nil(t, b.lease("a"))
	})

	t.Run("a slot that is not a whole number of seconds", func(t *testing.T) {
		// 4.5 s slots from genesis on: 144 s epochs.
		b := newWorldBuilder(t)
		b.healthMod = func(n *fleet.NetworkHealth) {
			n.Phases = fleet.SinglePhase(testGenesis, 4500*time.Millisecond)
			n.SecondsPerSlot = 4

			current, _ := n.EpochAt(b.w.Now)
			n.CurrentEpoch = current
			n.FinalizedEpoch = current - 2
			n.LivenessEpoch = current - 1
		}
		b.refreshHealth()
		require.Equal(t, 144*time.Second, b.w.Health.EpochDuration())

		b.host("a", 10).Status.Node = nodeWith(8, 8)
		b.host("idle", 90)
		b.wants("a", packagesNeed("2.0"))
		b.step()
		b.finish("a")
		b.step()
		b.advance(5 * time.Second)
		b.step()
		assert.Equal(t, "node at head with peers, waiting for it to stay so for 2m24s", b.lease("a").Readiness)

		b.advance(2 * 144 * time.Second)
		b.step()
		assert.Nil(t, b.lease("a"))
	})

	t.Run("an epoch that cannot be placed fails safe", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.get("a").Status.Node = nodeWith(8, 8)
		b.healthMod = func(n *fleet.NetworkHealth) { n.Phases = nil }
		b.advance(5 * time.Second)
		b.step()
		b.advance(3 * testEpoch)

		out := b.step()
		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "epoch timing unknown", b.lease("a").Readiness)
		assert.False(t, out.Plan.Gate.OK)

		// No repair without it either.
		b.down("other1")
		assert.False(t, newPass(b.w).offline(newPass(b.w).hosts["other1"]))
	})
}

// Finding 5: a run of healthy syncs with a gap of more than hostStaleAfter is
// two runs. Before, a healthy sync right after the operation, ten minutes of
// silence and one more healthy sync released the host.
func TestReview3Finding5SilenceEndsTheHealthyRun(t *testing.T) {
	b := verifying(t, 5, 0)
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a").SyncedSince)

	// Ten minutes of silence: the agent is stale, the run is over.
	b.advance(10*time.Minute, "a")
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Equal(t, "agent silent", b.lease("a").Readiness)
	assert.Nil(t, b.lease("a").SyncedSince)

	// One more healthy sync starts a new run; it does not release.
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Equal(t, b.w.Now, *b.lease("a").SyncedSince)
	assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", b.lease("a").Readiness)

	b.advance(testEpoch)
	b.step()
	assert.Nil(t, b.lease("a"))

	// The same when no reconcile saw the silence: the gap between two syncs
	// of the run is what counts.
	t.Run("a gap nobody watched", func(t *testing.T) {
		b := verifying(t, 5, 0)
		b.get("a").Status.Node = nodeWith(8, 8)
		b.advance(5 * time.Second)
		b.step()

		first := *b.lease("a").SyncedSince
		b.advance(10*time.Minute+5*time.Second, "a")
		b.get("a").LastSeen = b.w.Now
		b.step()

		require.NotNil(t, b.lease("a"))
		assert.NotEqual(t, first, *b.lease("a").SyncedSince)
		assert.Equal(t, b.w.Now, *b.lease("a").SyncedSince)
	})

	// A controller restart ends the run as well: the first sync after it does
	// not release.
	t.Run("a controller restart", func(t *testing.T) {
		b := verifying(t, 5, 0)
		b.get("a").Status.Node = nodeWith(8, 8)
		b.advance(5 * time.Second)
		b.step()

		b.advance(testEpoch+time.Minute, "a", "other1", "other2", "other3", "other4")
		b.w.Since = b.w.Now

		b.step()
		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "waiting for a sync after the controller's start", b.lease("a").Readiness)

		b.advance(5 * time.Second)
		b.step()
		require.NotNil(t, b.lease("a"), "the run begins anew after the start")
		assert.Equal(t, b.w.Now, *b.lease("a").SyncedSince)

		b.advance(testEpoch)
		b.step()
		assert.Nil(t, b.lease("a"))
	})
}

// Finding 6, the small ones.
func TestReview3Finding6SmallOnes(t *testing.T) {
	t.Run("no repair in the pass in which a repair lease ended", func(t *testing.T) {
		b := newWorldBuilder(t)

		for i := range 10 {
			name := fmt.Sprintf("h%d", i)
			b.host(name, 10)
			b.wants(name, packagesNeed("2.0"))
		}

		b.down("h4")
		b.down("h8")
		b.step()
		require.Equal(t, []string{"h4"}, b.leasedHosts())

		// The agent refuses the lease: it expires without having stopped
		// anything. The next repair does not begin in that pass.
		b.report("h4", protocol.PhaseDone, false, protocol.StepRefused)

		out := b.step()
		require.Contains(t, auditActions(out), "lease.expired")
		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, "after repair of h4", queueReasons(out.Plan)["h8"])

		b.advance(time.Second)
		b.get("h4").HoldWanted = true
		b.step()
		assert.Equal(t, []string{"h8"}, b.leasedHosts(), "a reconcile later")
	})

	t.Run("an adopted lease under a closed gate counts as a repair", func(t *testing.T) {
		b := newWorldBuilder(t)

		for i := range 10 {
			name := fmt.Sprintf("h%d", i)
			b.host(name, 10)
			b.wants(name, packagesNeed("2.0"))
		}

		b.down("h4")
		b.down("h8")

		// h4 works on a lease this database does not know.
		b.get("h4").Status.Operation = &protocol.Operation{
			LeaseID: "lost", Phase: protocol.PhaseApplying, StartedAt: b.w.Now, Disrupted: true,
			Steps: []protocol.Step{{NeedID: packagesNeed("2.0").ID, Status: protocol.StepRunning}},
		}

		out := b.step()
		require.Contains(t, auditActions(out), "lease.adopted")
		require.Equal(t, []string{"h4"}, b.leasedHosts())
		assert.True(t, b.lease("h4").Repair, "the gate is closed: nobody knows it was not a repair")
		assert.Equal(t, "after repair of h4", queueReasons(out.Plan)["h8"])

		// With the gate open an adopted lease is an ordinary one.
		c := newWorldBuilder(t)
		c.host("x", 10)
		c.host("y", 90)
		c.wants("x", packagesNeed("2.0"))
		c.get("x").Status.Operation = &protocol.Operation{
			LeaseID: "lost", Phase: protocol.PhaseApplying, StartedAt: c.w.Now, Disrupted: true,
			Steps: []protocol.Step{{NeedID: packagesNeed("2.0").ID, Status: protocol.StepRunning}},
		}
		c.step()
		require.NotNil(t, c.lease("x"))
		assert.False(t, c.lease("x").Repair)
	})

	t.Run("a repair lease that is taken up again is still a repair", func(t *testing.T) {
		b := newWorldBuilder(t)

		for i := range 10 {
			name := fmt.Sprintf("h%d", i)
			b.host(name, 10)
			b.wants(name, packagesNeed("2.0"))
		}

		b.down("h4")
		b.down("h8")
		b.step()
		require.Equal(t, []string{"h4"}, b.leasedHosts())

		first := b.lease("h4").ID

		// The agent says nothing until after the grace period: the lease
		// expires. Then it turns out to have started after all.
		b.w.Settings.Paused = true
		b.advance(b.w.Settings.LeaseTTL.Std() + leaseStartGrace)
		b.step()
		require.Empty(t, b.leasedHosts())

		b.advance(5 * time.Second)
		b.get("h4").Status.Operation = &protocol.Operation{
			LeaseID: first, Phase: protocol.PhaseApplying, Disrupted: true,
			Steps: []protocol.Step{{NeedID: packagesNeed("2.0").ID, Status: protocol.StepRunning}},
		}
		b.w.Settings.Paused = false
		b.down("h8")

		out := b.step()
		require.Contains(t, auditActions(out), "lease.reopened")
		require.Equal(t, []string{"h4"}, b.leasedHosts())
		assert.True(t, b.lease("h4").Repair)
		assert.Equal(t, "after repair of h4", queueReasons(out.Plan)["h8"])
	})

	t.Run("a forced lease through the closed gate is a repair too", func(t *testing.T) {
		b := newWorldBuilder(t)

		for i := range 10 {
			name := fmt.Sprintf("h%d", i)
			b.host(name, 10)
			b.wants(name, packagesNeed("2.0"))
		}

		b.down("h4")
		b.down("h8")
		b.w.Settings.Paused = true
		b.step()
		b.mustRollout(packagesKey("2.0")).Force = true
		b.w.Settings.Paused = false

		out := b.step()
		require.Equal(t, []string{"h4"}, b.leasedHosts())
		assert.True(t, b.lease("h4").Force)
		assert.True(t, b.lease("h4").Repair)
		assert.Equal(t, "after repair of h4", queueReasons(out.Plan)["h8"])
	})

	t.Run("a released member that is not done carries no lease ID", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 0)
		b.wants("a", packagesNeed("2.0"), rebootNeed())
		b.step()
		require.Len(t, b.lease("a").Needs, 2)

		// The reboot is done, the packages step was superseded: still needed.
		statuses := make([]protocol.StepStatus, 0, 2)

		for _, n := range b.lease("a").Needs {
			if n.Kind == protocol.NeedReboot {
				statuses = append(statuses, protocol.StepDone)
			} else {
				statuses = append(statuses, protocol.StepSuperseded)
			}
		}

		b.report("a", protocol.PhaseDone, true, statuses...)
		b.get("a").Status.Needs = []protocol.Need{packagesNeed("2.0")}
		b.w.Settings.Paused = true
		b.step()
		b.advance(5 * time.Second)
		b.step()
		b.advance(2 * testEpoch)

		out := b.step()
		require.Contains(t, auditActions(out), "lease.released")

		m := b.mustRollout(packagesKey("2.0")).Host("a")
		assert.Equal(t, fleet.HostPending, m.State)
		assert.Empty(t, m.LeaseID)
		assert.Equal(t, fleet.RolloutCompleted, b.closed(keyReboot).State)
	})

	t.Run("an epoch that begins when the last lease ended does not count", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 10)
		b.host("b", 90)
		b.down("a")

		began, ok := b.w.Health.EpochStart(b.w.Health.LivenessEpoch - 1)
		require.True(t, ok)

		b.w.LastLeaseEnd = map[string]time.Time{"a": began}
		assert.False(t, newPass(b.w).offline(newPass(b.w).hosts["a"]), "equal is not after")

		b.w.LastLeaseEnd = map[string]time.Time{"a": began.Add(-time.Millisecond)}
		assert.True(t, newPass(b.w).offline(newPass(b.w).hosts["a"]))
	})

	t.Run("a done member whose lease is known to have failed is no success", func(t *testing.T) {
		// A row as an older build wrote it: done, with the ID of a lease that
		// failed. The host still reports that lease's operation, so the lease
		// is at hand.
		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 100

		for _, name := range []string{"a", "b", "c"} {
			b.host(name, 10)
			b.wants(name, packagesNeed("2.0"))
		}

		b.w.Settings.Paused = true
		b.step()

		r := b.mustRollout(packagesKey("2.0"))
		r.Host("a").State = fleet.HostDone
		r.Host("a").LeaseID = "old"
		b.get("a").Status.Needs = nil
		b.w.ClosedLeases["old"] = &fleet.Lease{ID: "old", Host: "a", State: fleet.LeaseFailed}
		b.w.Settings.Paused = false

		out := b.step()
		assert.Equal(t, []string{"b"}, b.leasedHosts(), "one canary: there was no success")
		assert.Equal(t, "after canary b", queueReasons(out.Plan)["c"])

		// Released, it is one.
		b.w.ClosedLeases["old"].State = fleet.LeaseReleased
		b.report("b", protocol.PhaseApplying, true)
		b.step()
		assert.Equal(t, []string{"b", "c"}, b.leasedHosts())
	})
}

// Found by the simulation of the third review: a controller restart lifted
// the skip of a host that had been silent since before it reported the need.
// After a restart such a host is not stale yet (silence counts from the
// start), and its old sync lists no need: it looked as if the need had gone.
// Only a sync from after the skip says so.
func TestReview3SkipIsNotLiftedByASyncFromBeforeIt(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.host("b", 0)
	b.wants("a", packagesNeed("2.0"))
	b.wants("b", packagesNeed("2.0"))
	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())

	key := packagesKey("2.0")
	skipped := b.mustRollout(key).Host("b")
	skipped.State = fleet.HostSkipped
	skipped.Error = skippedPrefix + " later"
	skipped.UpdatedAt = b.w.Now

	b.serve("a")
	require.Equal(t, fleet.RolloutCompleted, b.closed(key).State)
	require.True(t, operatorSkipped(b.closed(key).Host("b")))

	// The controller restarts. "b" has not synced since long before the
	// skip; what it said then lists no need.
	b.get("b").LastSeen = skipped.UpdatedAt.Add(-time.Hour)
	b.get("b").Status.Needs = nil
	b.w.Since = b.w.Now

	out := b.step()
	assert.NotContains(t, auditActions(out), "rollout.skip-lifted")
	assert.True(t, operatorSkipped(b.closed(key).Host("b")), "an old sync says nothing about the need")

	// It syncs, without the need: now the need is gone.
	b.advance(5 * time.Second)

	out = b.step()
	assert.Contains(t, auditActions(out), "rollout.skip-lifted")
}
