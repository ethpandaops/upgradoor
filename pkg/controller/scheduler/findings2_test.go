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

// The tests in this file replay the scenarios of the second review of the
// scheduler, one per finding (finding 9, the stage of a member, is asserted
// where stages are: TestCanaryCandidates and the tests of the first review).

// Finding 1, first variant: one epoch in which no attestation is seen does
// not make every validator host a repair. Before, every host with a row of
// zero was a broken host and passed the closed gate, the canary and the
// weight budget; only the host limit bounded it.
func TestReview2Finding1OneEpochWithoutAttestationsGrantsNothing(t *testing.T) {
	b := newWorldBuilder(t)

	names := make([]string, 0, 30)

	for i := range 30 {
		name := fmt.Sprintf("h%02d", i)
		names = append(names, name)
		b.host(name, 10)
		b.wants(name, packagesNeed("2.0"))
	}

	// The newest epoch shows nobody attesting; the one before was fine.
	b.fillLiveness()

	for _, name := range names {
		b.live(b.w.Health.LivenessEpoch-1, name, 10)
		b.live(b.w.Health.LivenessEpoch, name, 0)
	}

	out := b.step()
	require.False(t, out.Plan.Gate.OK)
	assert.Equal(t, []string{"participation 0% < 90%"}, out.Plan.Gate.Reasons)
	assert.Empty(t, b.leasedHosts(), "a glitch of one epoch is no reason to repair anybody")

	for _, q := range out.Plan.Queue {
		assert.Equal(t, WaitGate, q.Wait, q.Host)
	}

	// The picture stays: one host at a time goes through the gate, and each
	// has to pass readiness before the next.
	for _, name := range names {
		b.liveOverride[name] = 0
	}

	b.advance(testEpoch)

	out = b.step()
	require.Len(t, b.leasedHosts(), 1, "one repair at a time, whatever the host limit")
	assert.Equal(t, 3, out.Plan.Budget.MaxLeasedHosts)

	first := b.leasedHosts()[0]
	assert.True(t, b.lease(first).Repair)

	b.advance(testEpoch)
	b.step()
	assert.Equal(t, []string{first}, b.leasedHosts(), "nobody else while the repair is open")

	// It comes back and is released: the next one may go.
	delete(b.liveOverride, first)
	b.serve(first)
	b.advance(testEpoch)
	b.step()

	require.Len(t, b.leasedHosts(), 1)
	assert.NotEqual(t, first, b.leasedHosts()[0])
}

// Finding 1, second variant: hosts with one validator each. Two of them miss
// their attestation in the epoch after the canary was leased. Before, both
// were leased as repairs through the closed gate: three of nine down before
// the first success.
func TestReview2Finding1MissedAttestationsAreNoRepairs(t *testing.T) {
	b := newWorldBuilder(t)

	for i := range 9 {
		name := fmt.Sprintf("h%d", i)
		b.host(name, 1)
		b.wants(name, packagesNeed("2.0"))
	}

	b.step()
	require.Equal(t, []string{"h0"}, b.leasedHosts(), "the canary")
	b.report("h0", protocol.PhaseApplying, true)

	b.advance(testEpoch)
	b.liveOverride["h0"] = 0
	b.live(b.w.Health.LivenessEpoch, "h3", 0)
	b.live(b.w.Health.LivenessEpoch, "h5", 0)

	out := b.step()
	require.False(t, out.Plan.Gate.OK, "6 of the 8 hosts without a lease attest")
	assert.Equal(t, []string{"h0"}, b.leasedHosts())
	assert.Equal(t, "network gate: participation 75% < 90%", queueReasons(out.Plan)["h3"])
	assert.Equal(t, "network gate: participation 75% < 90%", queueReasons(out.Plan)["h5"])
}

// Finding 2: a controller outage does not fail a verifying lease. The
// readiness timeout runs from the scheduler's start at the earliest; before,
// it ran from the agent's done report, and since a host with validators
// cannot be ready while liveness is unknown, the first reconcile after 31
// minutes of downtime failed the lease and halted the rollout.
func TestReview2Finding2OutageDoesNotFailAVerifyingLease(t *testing.T) {
	b := verifying(t, 5, 10)
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	require.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	// The controller is down for 31 minutes. It starts again; nobody has
	// synced yet and the beacon service has no liveness yet.
	b.w.Now = b.w.Now.Add(31 * time.Minute)
	b.w.Since = b.w.Now
	b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
	b.refreshHealth()

	out := b.step()
	require.NotNil(t, b.lease("a"), "the outage alone fails no lease")
	assert.Equal(t, fleet.LeaseVerifying, b.lease("a").State)
	assert.NotContains(t, auditActions(out), "lease.failed")
	assert.Equal(t, fleet.RolloutActive, b.mustRollout(packagesKey("2.0")).State)

	// Everything comes back: the host is released as usual.
	b.healthMod = nil
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)
	b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)

	// A host that does not come back still fails, readinessTimeout after the
	// start.
	t.Run("the timeout runs from the start", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.liveOverride["a"] = 0
		b.w.Now = b.w.Now.Add(31 * time.Minute)
		b.w.Since = b.w.Now
		b.refreshHealth()
		b.step()
		require.NotNil(t, b.lease("a"))

		b.advance(b.w.Settings.ReadinessTimeout.Std())
		b.step()
		require.NotNil(t, b.lease("a"), "not before the timeout has passed since the start")

		b.advance(time.Second)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Contains(t, auditActions(out), "lease.failed")
	})
}

// review2Finding3 builds four validator hosts in an image rollout whose
// canary "a" is out, while "d" holds a lease of the reboot rollout.
func review2Finding3(t *testing.T) *worldBuilder {
	t.Helper()

	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.ReadyEpochs = 0

	for _, name := range []string{"a", "b", "c", "d"} {
		b.host(name, 32)
		b.wantsImage(name, imgLighthouse, digestA)
	}

	b.wants("d", rebootNeed())
	b.step()
	require.Equal(t, []string{"a", "d"}, b.leasedHosts())
	require.Equal(t, "a", b.mustRollout(imageKey(imgLighthouse, digestA)).Canary)
	require.Len(t, b.lease("d").Needs, 1, "the reboot alone: in the image rollout d waits for the canary")

	b.report("a", protocol.PhaseApplying, true)
	b.report("d", protocol.PhaseRebooting, true)
	b.step()

	return b
}

// Finding 3, first variant: a host that another rollout has just served is
// not a repair here, although its liveness row still shows the outage of the
// operation. Before, d was leased in the image rollout through the closed
// gate while the canary was still out.
func TestReview2Finding3HostJustServedElsewhereIsNoRepair(t *testing.T) {
	b := review2Finding3(t)
	key := imageKey(imgLighthouse, digestA)

	// The reboot takes two epochs, in which d does not attest. With
	// readyEpochs 0 its lease is released at the first healthy sync.
	b.advance(2 * testEpoch)
	b.down("d")
	b.report("a", protocol.PhaseApplying, true)
	b.finish("d")
	b.step()
	b.advance(5 * time.Second)
	b.report("a", protocol.PhaseApplying, true)

	out := b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts(), "d is released and not leased again")
	require.False(t, out.Plan.Gate.OK)
	assert.Equal(t, "network gate: participation 66.6% < 90%", queueReasons(out.Plan)["d"])
	assert.Equal(t, fleet.HostPending, b.mustRollout(key).Host("d").State)

	// Its rows of the epochs of the operation do not count either, however
	// long they stay the newest.
	b.advance(time.Minute)
	b.report("a", protocol.PhaseApplying, true)
	b.step()
	assert.Equal(t, []string{"a"}, b.leasedHosts())
}

// Finding 3, second variant: the other rollout's lease fails. Before, the
// same reconcile granted d again as a repair that costs nothing.
func TestReview2Finding3HostWhoseOtherLeaseFailedIsNoRepair(t *testing.T) {
	b := review2Finding3(t)

	b.advance(2 * testEpoch)
	b.down("d")
	b.report("a", protocol.PhaseApplying, true)
	b.report("d", protocol.PhaseFailed, true)

	out := b.step()
	require.Contains(t, auditActions(out), "lease.failed")
	assert.Equal(t, []string{"a"}, b.leasedHosts(), "not granted again in the reconcile that failed its lease")
	assert.Equal(t, fleet.RolloutHalted, b.mustRollout(keyReboot).State)

	// Two complete epochs after the failure it is established as offline and
	// may be repaired through the gate, one host at a time.
	for range 2 {
		b.advance(testEpoch)
		b.report("a", protocol.PhaseApplying, true)
		b.step()
		require.Equal(t, []string{"a"}, b.leasedHosts(), "an epoch that began before the failure does not count")
	}

	b.advance(testEpoch)
	b.report("a", protocol.PhaseApplying, true)
	b.step()
	require.Equal(t, []string{"a", "d"}, b.leasedHosts())
	assert.True(t, b.lease("d").Repair)
}

// Finding 4: a member whose step was done in a lease that failed on another
// step is no success of its rollout. Before, the canary counted as the first
// success without having passed readiness, and the rest went.
func TestReview2Finding4StepDoneInAFailedLeaseIsNoSuccess(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100

	for _, name := range []string{"a", "b", "c"} {
		b.host(name, 10)
		b.wantsImage(name, imgLighthouse, digestA)
	}

	b.wants("a", rebootNeed())
	b.step()

	key := imageKey(imgLighthouse, digestA)
	require.Equal(t, []string{"a"}, b.leasedHosts())
	require.Equal(t, "a", b.mustRollout(key).Canary)
	require.Len(t, b.lease("a").Needs, 2)

	// The image step is done, the reboot fails.
	statuses := make([]protocol.StepStatus, 0, 2)

	for _, n := range b.lease("a").Needs {
		if n.Kind == protocol.NeedImage {
			statuses = append(statuses, protocol.StepDone)
		} else {
			statuses = append(statuses, protocol.StepFailed)
		}
	}

	b.report("a", protocol.PhaseFailed, true, statuses...)
	b.get("a").Status.Needs = []protocol.Need{rebootNeed()}
	b.get("a").Status.Containers = []protocol.Container{
		container("c-"+imgLighthouse, imgLighthouse, digestA, digestA, b.w.Now),
	}

	out := b.step()
	r := b.mustRollout(key)

	require.Equal(t, fleet.RolloutHalted, b.mustRollout(keyReboot).State)
	require.Equal(t, fleet.RolloutActive, r.State, "the image step was done: the image rollout is not halted")
	assert.Equal(t, fleet.HostDone, r.Host("a").State)
	assert.Empty(t, r.Host("a").LeaseID, "done, but not through a lease that was released")

	// No first success: one new canary goes alone.
	assert.Equal(t, []string{"b"}, b.leasedHosts())
	assert.Equal(t, "b", r.Canary)
	assert.Equal(t, map[string]string{"c": "after canary b"}, queueReasons(out.Plan))

	b.serve("b")
	assert.Equal(t, []string{"c"}, b.leasedHosts())
}

// Finding 5: a host whose validators never attest, with a working agent, is
// an ordinary member. Before, it was a broken host: leased first in every
// rollout it was in, failed readiness and halted the rollout with nothing
// done, again after every retry.
func TestReview2Finding5DeadHostIsNeitherFirstNorCanary(t *testing.T) {
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
	assert.False(t, b.lease("dead").Repair)
}

// Finding 6: a broken host stays first through pending, expected and pending
// again. Before, the mark was a prefix of the member's error note, which the
// change to expected cleared: held and unheld, the host came back as an
// ordinary member and could become the canary. Now there is no mark: the
// host's last lease for the image failed, whatever the member's state.
func TestReview2Finding6BrokenHostStaysFirstThroughStateChanges(t *testing.T) {
	b := failedOnDigestA(t, "h3", map[string]uint64{"h1": 10, "h2": 10, "h3": 10})
	first := b.closedOrOpen(imageKey(imgLighthouse, digestA)).ID
	note := brokenBy(b.lastLease("h3").ID, first)
	b.advance(time.Minute)
	seesDigestB(b, "h1", "h2", "h3")

	// Nothing is granted meanwhile.
	b.w.Settings.Paused = true
	b.step()

	key := imageKey(imgLighthouse, digestB)
	require.Equal(t, note, b.mustRollout(key).Host("h3").Error)
	require.Equal(t, fleet.StageRepair, b.mustRollout(key).Host("h3").Stage)
	require.Equal(t, fleet.HostPending, b.mustRollout(key).Host("h3").State)

	// The host goes on hold: a held agent reports no need.
	needs := b.get("h3").Status.Needs
	b.get("h3").HoldWanted = true
	b.get("h3").Status.Needs = nil
	b.step()
	require.Equal(t, fleet.HostExpected, b.mustRollout(key).Host("h3").State)
	assert.Equal(t, note, b.mustRollout(key).Host("h3").Error)
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

	// Served, it is in order: the note is gone, and the stage says how it was
	// served. Its repair was the first success, so nobody waits for a canary.
	b.serve("h3")
	r = b.mustRollout(key)
	assert.Equal(t, fleet.HostDone, r.Host("h3").State)
	assert.Empty(t, r.Host("h3").Error)
	assert.Equal(t, fleet.StageRepair, r.Host("h3").Stage)
	assert.Nil(t, newLedger(b.w.Ended).brokenSince("h3", rolloutGroup(r)))
	assert.Equal(t, []string{"h1"}, b.leasedHosts())
}

// Finding 7: the run of healthy syncs must span the window. Before, one
// healthy sync after the operation and an epoch of silence released the
// host: only the time since that sync was looked at.
func TestReview2Finding7HealthyRunMustSpanTheWindow(t *testing.T) {
	b := verifying(t, 5, 0)
	// An epoch is shorter than hostStaleAfter: the silence is not staleness.
	b.w.Settings.HostStaleAfter = fleet.Duration(3 * testEpoch)
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a").SyncedSince)

	// No sync from the host for an epoch and more.
	b.advance(testEpoch+time.Minute, "a")
	b.step()
	require.NotNil(t, b.lease("a"), "an old sync says nothing about now")
	assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", b.lease("a").Readiness)

	// It syncs again, still healthy: the run now spans the window.
	b.advance(5 * time.Second)

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, "ready: node at head with peers", releasedReadiness(t, out))
}

// Finding 8: the attestation bar is the smallest count that is at least
// minParticipationPercent of the host's validators. Before, it was rounded
// down: 1 of 2, 2 of 3, 8 of 9 validators were enough at 90%.
func TestReview2Finding8AttestationBarRoundsUp(t *testing.T) {
	for _, tt := range []struct {
		percent float64
		weight  uint64
		want    uint64
	}{
		{90, 1, 1}, {90, 2, 2}, {90, 3, 3}, {90, 9, 9}, {90, 10, 9}, {90, 11, 10},
		{90, 128, 116}, {90, 100, 90}, {66, 3, 2}, {50, 3, 2}, {50, 4, 2},
		{100, 5, 5}, {0, 5, 1}, {33.3, 3, 1}, {80, 5, 4},
	} {
		assert.Equal(t, tt.want, attestationBar(tt.percent, tt.weight), "%v%% of %d", tt.percent, tt.weight)
	}

	// One of two validators attesting is 50%, not 90%.
	b := verifying(t, 5, 2)
	b.liveOverride["a"] = 1
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	b.advance(3 * testEpoch)
	b.step()

	require.NotNil(t, b.lease("a"))
	assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 2 of 2", b.lease("a").Readiness)

	delete(b.liveOverride, "a")
	b.advance(testEpoch)
	b.step()
	assert.Nil(t, b.lease("a"))
}
