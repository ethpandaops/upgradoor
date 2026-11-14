package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestBudgetAdmit(t *testing.T) {
	tests := []struct {
		name   string
		budget budgetState
		cost   uint64
		want   string
	}{
		{
			name:   "fits exactly",
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 4, leased: 1, maxLeased: 3},
			cost:   6,
		},
		{
			name:   "one validator too many",
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 5, leased: 1, maxLeased: 3},
			cost:   6,
			want:   "budget 5% of 10%",
		},
		{
			name:   "heavy host goes alone when nothing is unavailable",
			budget: budgetState{total: 100, percent: 10, allowed: 10, maxLeased: 1},
			cost:   40,
		},
		{
			name:   "heavy host waits while a host without validators holds a lease",
			budget: budgetState{total: 100, percent: 10, allowed: 10, leased: 1, maxLeased: 3},
			cost:   40,
			want:   "budget 0% of 10%",
		},
		{
			name:   "heavy host waits while one validator is down elsewhere",
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 1, maxLeased: 3},
			cost:   40,
			want:   "budget 1% of 10%",
		},
		{
			name:   "already-down host is free",
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 50, maxLeased: 1},
			cost:   0,
		},
		{
			name:   "zero maxUnavailable never lets a live validator go",
			budget: budgetState{total: 100, maxLeased: 1},
			cost:   1,
			want:   "budget 0% of 0%",
		},
		{
			name:   "zero maxUnavailable still serves a host that costs nothing",
			budget: budgetState{total: 100, maxLeased: 1},
			cost:   0,
		},
		{
			name:   "host limit",
			budget: budgetState{total: 100, percent: 10, allowed: 10, leased: 2, maxLeased: 2},
			cost:   0,
			want:   "host limit 2 of 2",
		},
		{
			name:   "budget is named before the host limit",
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 10, leased: 2, maxLeased: 2},
			cost:   1,
			want:   "budget 10% of 10%",
		},
		{
			name:   "fractional percent",
			budget: budgetState{total: 8, percent: 12.5, allowed: 1, leased: 0, maxLeased: 1},
			cost:   1,
		},
		{
			name:   "percent is rounded down in the reason",
			budget: budgetState{total: 3, percent: 12.5, allowed: 0.375, unavailable: 1, maxLeased: 1},
			cost:   1,
			want:   "budget 33.3% of 12.5%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.budget.admit(tt.cost)

			assert.Equal(t, tt.want, v.reason)
			assert.Equal(t, tt.want == "", v.rank == rankGrantable)
		})
	}
}

func TestBudgetFromWorld(t *testing.T) {
	b := newWorldBuilder(t)
	b.autoLive = false
	b.host("full", 10)
	b.host("partial", 10)
	b.host("leased", 10)
	b.host("norow", 7)
	b.host("tooling", 0)

	epoch := b.w.Health.LivenessEpoch
	b.live(epoch, "full", 10)
	b.live(epoch, "partial", 4)
	b.live(epoch, "leased", 10)
	b.live(epoch-1, "norow", 0) // an older epoch does not count
	b.w.Leases = []*fleet.Lease{{ID: "l1", Host: "leased", State: fleet.LeaseVerifying, AgentDoneAt: &b.w.Now}}

	plan := Reconcile(b.w).Plan

	assert.Equal(t, Budget{
		TotalWeight:       37,
		AllowedWeight:     3.7,
		UnavailableWeight: 16, // 6 not attesting on "partial" plus all of "leased"
		LeasedHosts:       1,
		MaxLeasedHosts:    1,
	}, plan.Budget)
	assert.Equal(t, HostPlan{Slot: SlotIdle, Weight: 7, Live: 7}, plan.Hosts["norow"])
	assert.Equal(t, HostPlan{Slot: SlotIdle, Weight: 10, Live: 4, LiveKnown: true}, plan.Hosts["partial"])
	assert.Equal(t, SlotVerifying, plan.Hosts["leased"].Slot)
}

func TestBudgetHeavyHostGoesAlone(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("canary", 1)
	b.host("mid", 39)
	b.host("big", 60)

	for _, h := range []string{"canary", "mid", "big"} {
		b.wants(h, packagesNeed("2.0"))
	}

	out := b.step()
	require.Equal(t, []string{"canary"}, b.leasedHosts())
	assert.Equal(t, map[string]string{"mid": "after canary canary", "big": "after canary canary"},
		queueReasons(out.Plan))

	b.serve("canary")
	// "mid" is far above the 10 allowed, but nothing is unavailable.
	require.Equal(t, []string{"mid"}, b.leasedHosts())
	assert.Equal(t, uint64(39), b.lease("mid").Charged)

	out = b.step()
	assert.Equal(t, map[string]string{"big": "budget 39% of 10%"}, queueReasons(out.Plan))

	b.serve("mid")
	require.Equal(t, []string{"big"}, b.leasedHosts())
}

func TestBudgetAlreadyDownHostIsFree(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MinParticipationPercent = 50
	b.host("down", 20)
	b.host("up", 20)
	b.host("idle", 60)
	b.liveOverride["down"] = 0
	b.wants("down", packagesNeed("2.0"))
	b.wants("up", packagesNeed("2.0"))

	out := b.step()

	// 20 of 100 are down already, twice the 10 allowed. Rolling the host
	// that is down costs nothing more.
	assert.Equal(t, []string{"down"}, b.leasedHosts())
	assert.Equal(t, uint64(0), b.lease("down").Charged)
	assert.Equal(t, uint64(20), b.lease("down").Weight)
	assert.Equal(t, uint64(20), out.Plan.Budget.UnavailableWeight)
	assert.Equal(t, "after canary down", queueReasons(out.Plan)["up"],
		"the only host that fits the budget is the canary")

	// A second cause shows the budget for the host that is up.
	b.wants("up", rebootNeed())

	out = b.step()
	assert.Equal(t, "budget 20% of 10%", queueReasons(out.Plan)["up"])
}

func TestBudgetZeroMaxUnavailable(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 0
	b.host("tool1", 0)
	b.host("tool2", 0)
	b.host("val", 10)

	for _, h := range []string{"tool1", "tool2", "val"} {
		b.wants(h, packagesNeed("2.0"))
	}

	// No validator may go down: the canary is a host that costs nothing, the
	// hosts without validators roll one at a time, the host with validators
	// waits for the budget.
	out := b.step()
	assert.Equal(t, []string{"tool1"}, b.leasedHosts())
	assert.Equal(t, "tool1", b.mustRollout(packagesKey("2.0")).Canary)
	assert.Equal(t, map[string]string{
		"val": "after canary tool1", "tool2": "after canary tool1",
	}, queueReasons(out.Plan))

	b.serve("tool1")
	assert.Equal(t, []string{"tool2"}, b.leasedHosts())

	b.serve("tool2")

	out = b.step()
	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, map[string]string{"val": "budget 0% of 0%"}, queueReasons(out.Plan))
}

// TestHostsThatCostNothingPassAFullBudget: in the rest, a host without
// validators is served while a host with validators waits for the budget.
func TestHostsThatCostNothingPassAFullBudget(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 50
	b.w.Settings.MinParticipationPercent = 50
	b.host("sick", 100)
	b.live(b.w.Health.LivenessEpoch, "sick", 60) // 40 of 200 down by themselves
	b.liveOverride["sick"] = 60

	for name, weight := range map[string]uint64{"canary": 10, "heavy": 90, "tool1": 0, "tool2": 0} {
		b.host(name, weight)
		b.wants(name, packagesNeed("2.0"))
	}

	b.step()
	require.Equal(t, []string{"canary"}, b.leasedHosts())
	b.serve("canary")

	out := b.step()
	assert.Equal(t, []string{"tool1", "tool2"}, b.leasedHosts())
	assert.Equal(t, map[string]string{"heavy": "budget 20% of 50%"}, queueReasons(out.Plan))
}

func TestHostLimitBoundsHostsWithoutValidators(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 25

	for _, h := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8"} {
		b.host(h, 0)
		b.wants(h, packagesNeed("2.0"))
	}

	b.step()
	require.Equal(t, []string{"t1"}, b.leasedHosts(), "the canary")
	b.serve("t1")

	out := b.step()
	assert.Equal(t, []string{"t2", "t3"}, b.leasedHosts())
	assert.Equal(t, 2, out.Plan.Budget.MaxLeasedHosts)
	assert.Equal(t, "host limit 2 of 2", queueReasons(out.Plan)["t4"])
	assert.Equal(t, 1, out.Plan.Queue[0].Position)
	assert.Equal(t, "t4", out.Plan.Queue[0].Host)
	assert.Equal(t, WaitHostLimit, out.Plan.Queue[0].Wait)
}

// Every reason a host can wait for has a wait kind: a client that sorts the
// queue into "starts by itself" and "needs somebody" must never see a blank.
func TestEveryWaitingRankHasAKind(t *testing.T) {
	seen := make(map[WaitKind]bool, len(waitKinds))

	for rank := rankHeld; rank <= rankNext; rank++ {
		kind, ok := waitKinds[rank]
		require.True(t, ok, "rank %d has no wait kind", rank)
		assert.NotEmpty(t, kind)
		assert.False(t, seen[kind], "wait kind %q is used twice", kind)

		seen[kind] = true
	}

	assert.NotContains(t, waitKinds, rankGrantable)
}

func TestGate(t *testing.T) {
	tests := []struct {
		name  string
		setup func(b *worldBuilder)
		want  []string
	}{
		{name: "healthy", setup: func(*worldBuilder) {}, want: []string{}},
		{
			name:  "never succeeded",
			setup: func(b *worldBuilder) { b.w.Health = fleet.NetworkHealth{} },
			want:  []string{"beacon node unreachable", "liveness data stale"},
		},
		{
			name: "view older than two epochs",
			setup: func(b *worldBuilder) {
				b.w.Health.UpdatedAt = b.w.Now.Add(-2*testEpoch - time.Second)
			},
			want: []string{"beacon node unreachable"},
		},
		{
			name: "view two epochs old is still good",
			setup: func(b *worldBuilder) {
				b.w.Health.UpdatedAt = b.w.Now.Add(-2 * testEpoch)
			},
			want: []string{},
		},
		{
			name: "finality lag",
			setup: func(b *worldBuilder) {
				b.w.Health.FinalityLagEpochs = 5
			},
			want: []string{"finality lag 5 epochs > 3"},
		},
		{
			name: "participation",
			setup: func(b *worldBuilder) {
				b.live(b.w.Health.LivenessEpoch, "a", 38) // 88 of 100
			},
			want: []string{"participation 88% < 90%"},
		},
		{
			name: "participation exactly at the floor",
			setup: func(b *worldBuilder) {
				b.live(b.w.Health.LivenessEpoch, "a", 40)
			},
			want: []string{},
		},
		{
			name: "validators of a leased host are left out on both sides",
			setup: func(b *worldBuilder) {
				// "a" is down under a lease: 50 of 50 remaining attest.
				b.live(b.w.Health.LivenessEpoch, "a", 0)
				b.w.Leases = []*fleet.Lease{{ID: "l1", Host: "a", State: fleet.LeaseRunning}}
				b.get("a").Status.Operation = &protocol.Operation{LeaseID: "l1", Phase: protocol.PhaseApplying}
			},
			want: []string{},
		},
		{
			name: "liveness unknown with tracked validators",
			setup: func(b *worldBuilder) {
				b.w.Health.LivenessKnown = false
			},
			want: []string{"liveness data stale"},
		},
		{
			name: "liveness not served by the beacon node: finality alone gates",
			setup: func(b *worldBuilder) {
				b.w.Health.LivenessKnown = false
				b.w.Health.LivenessUnsupported = true
			},
			want: []string{},
		},
		{
			name: "liveness not served: rows of an earlier time say nothing",
			setup: func(b *worldBuilder) {
				b.w.Health.LivenessKnown = false
				b.w.Health.LivenessUnsupported = true
				b.w.Health.CurrentEpoch += 20
				b.live(b.w.Health.LivenessEpoch, "a", 0)
			},
			want: []string{},
		},
		{
			name: "liveness not served: the finality lag still closes the gate",
			setup: func(b *worldBuilder) {
				b.w.Health.LivenessKnown = false
				b.w.Health.LivenessUnsupported = true
				b.w.Health.FinalityLagEpochs = 4
			},
			want: []string{"finality lag 4 epochs > 3"},
		},
		{
			name: "liveness not served: a stale beacon view still closes the gate",
			setup: func(b *worldBuilder) {
				b.w.Health.LivenessKnown = false
				b.w.Health.LivenessUnsupported = true
				b.w.Health.UpdatedAt = b.w.Now.Add(-2*testEpoch - time.Second)
			},
			want: []string{"beacon node unreachable"},
		},
		{
			name: "liveness more than two epochs behind",
			setup: func(b *worldBuilder) {
				b.w.Health.CurrentEpoch += 2
			},
			want: []string{"liveness data stale"},
		},
		{
			name: "several reasons",
			setup: func(b *worldBuilder) {
				b.w.Health.FinalityLagEpochs = 9
				b.live(b.w.Health.LivenessEpoch, "a", 0)
			},
			want: []string{"finality lag 9 epochs > 3", "participation 50% < 90%"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.host("a", 50)
			b.host("b", 50)
			b.live(b.w.Health.LivenessEpoch, "a", 50)
			b.live(b.w.Health.LivenessEpoch, "b", 50)
			b.autoLive = false
			tt.setup(b)

			gate := b.step().Plan.Gate

			assert.Equal(t, tt.want, gate.Reasons)
			assert.Equal(t, len(tt.want) == 0, gate.OK)
		})
	}
}

func TestGateWithoutValidatorsNeedsNoLiveness(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Health.LivenessKnown = false
	b.host("tool", 0)
	b.wants("tool", packagesNeed("2.0"))

	out := b.step()

	assert.True(t, out.Plan.Gate.OK)
	assert.Equal(t, []string{"tool"}, b.leasedHosts())
}

func TestGateStopsGrantsButOpenLeasesFinish(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("t1", 0)
	b.host("t2", 0)
	b.wants("t1", packagesNeed("2.0"))
	b.wants("t2", packagesNeed("2.0"))
	b.step()
	require.Equal(t, []string{"t1"}, b.leasedHosts())

	b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 7 }
	b.serve("t1")

	out := b.step()
	assert.Empty(t, b.leasedHosts(), "the open lease is released while the gate fails")
	assert.Equal(t, map[string]string{"t2": "network gate: finality lag 7 epochs > 3"},
		queueReasons(out.Plan))
}

// TestWaitingReasons pins the exact reason strings and their priority.
func TestWaitingReasons(t *testing.T) {
	tests := []struct {
		name  string
		setup func(b *worldBuilder)
		want  string
	}{
		{
			name:  "held",
			setup: func(b *worldBuilder) { b.get("h").HoldWanted = true },
			want:  "held",
		},
		{
			name:  "agent silent",
			setup: func(b *worldBuilder) { b.get("h").LastSeen = b.w.Now.Add(-3 * time.Minute) },
			want:  "agent silent",
		},
		{
			name:  "paused",
			setup: func(b *worldBuilder) { b.w.Settings.Paused = true },
			want:  "paused",
		},
		{
			name: "rollout paused",
			setup: func(b *worldBuilder) {
				b.w.Settings.PolicyPackages = fleet.PolicyPaused
				b.step()
				b.w.Settings.PolicyPackages = fleet.PolicyAuto
				b.mustRollout(packagesKey("2.0")).Paused = true
			},
			want: "rollout paused",
		},
		{
			name:  "waiting for approval",
			setup: func(b *worldBuilder) { b.w.Settings.PolicyPackages = fleet.PolicyManual },
			want:  "waiting for approval (policy manual)",
		},
		{
			name:  "policy paused",
			setup: func(b *worldBuilder) { b.w.Settings.PolicyPackages = fleet.PolicyPaused },
			want:  "policy paused",
		},
		{
			name: "rollout halted",
			setup: func(b *worldBuilder) {
				b.host("first", 5)
				b.wants("first", packagesNeed("2.0"))
				b.step()
				b.report("first", protocol.PhaseFailed, true)
			},
			want: "rollout halted: first: container did not start",
		},
		{
			name:  "network gate",
			setup: func(b *worldBuilder) { b.w.Health.FinalityLagEpochs = 5 },
			want:  "network gate: finality lag 5 epochs > 3",
		},
		{
			name: "after canary",
			setup: func(b *worldBuilder) {
				b.host("a", 5)
				b.wants("a", packagesNeed("2.0"))
			},
			want: "after canary a",
		},
		{
			name: "budget",
			setup: func(b *worldBuilder) {
				b.host("sick", 90)
				b.live(b.w.Health.LivenessEpoch, "sick", 82) // 8 of 100 down
				b.w.Settings.MinParticipationPercent = 50
			},
			want: "budget 8% of 10%",
		},
		{
			name: "held wins over everything else",
			setup: func(b *worldBuilder) {
				b.get("h").Status.Hold.Local = true
				b.get("h").LastSeen = b.w.Now.Add(-3 * time.Minute)
				b.w.Settings.Paused = true
				b.w.Settings.PolicyPackages = fleet.PolicyPaused
				b.w.Health.FinalityLagEpochs = 5
			},
			want: "held",
		},
		{
			name: "agent silent wins over paused",
			setup: func(b *worldBuilder) {
				b.get("h").LastSeen = b.w.Now.Add(-3 * time.Minute)
				b.w.Settings.Paused = true
			},
			want: "agent silent",
		},
		{
			name: "paused wins over policy and gate",
			setup: func(b *worldBuilder) {
				b.w.Settings.Paused = true
				b.w.Settings.PolicyPackages = fleet.PolicyManual
				b.w.Health.FinalityLagEpochs = 5
			},
			want: "paused",
		},
		{
			name: "rollout paused wins over approval",
			setup: func(b *worldBuilder) {
				b.w.Settings.PolicyPackages = fleet.PolicyManual
				b.step()
				b.mustRollout(packagesKey("2.0")).Paused = true
			},
			want: "rollout paused",
		},
		{
			name: "approval wins over the gate",
			setup: func(b *worldBuilder) {
				b.w.Settings.PolicyPackages = fleet.PolicyManual
				b.w.Health.FinalityLagEpochs = 5
			},
			want: "waiting for approval (policy manual)",
		},
		{
			name: "policy paused wins over halted",
			setup: func(b *worldBuilder) {
				b.host("first", 5)
				b.wants("first", packagesNeed("2.0"))
				b.step()
				b.report("first", protocol.PhaseFailed, true)
				b.w.Settings.PolicyPackages = fleet.PolicyPaused
			},
			want: "policy paused",
		},
		{
			name: "halted wins over the gate",
			setup: func(b *worldBuilder) {
				b.host("first", 5)
				b.wants("first", packagesNeed("2.0"))
				b.step()
				b.report("first", protocol.PhaseFailed, true)
				b.w.Health.FinalityLagEpochs = 5
			},
			want: "rollout halted: first: container did not start",
		},
		{
			name: "gate wins over ordering",
			setup: func(b *worldBuilder) {
				b.host("a", 5)
				b.wants("a", packagesNeed("2.0"))
				b.w.Health.FinalityLagEpochs = 5
			},
			want: "network gate: finality lag 5 epochs > 3",
		},
		{
			name: "verb not allowed",
			setup: func(b *worldBuilder) {
				b.get("h").Status.Verbs = []protocol.Verb{protocol.VerbRoll}
			},
			want: "verb not allowed by the agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.host("h", 10)
			b.wants("h", packagesNeed("2.0"))
			tt.setup(b)

			out := b.step()

			require.Nil(t, b.lease("h"))
			assert.Equal(t, tt.want, queueReasons(out.Plan)["h"])
			assert.Equal(t, tt.want, out.Plan.Hosts["h"].Reason)
		})
	}
}

func TestWaitingReasonShowsTheNeedClosestToAGrant(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyReboot = fleet.PolicyManual
	b.host("other", 90)
	b.host("h", 10)
	b.live(b.w.Health.LivenessEpoch, "other", 82)
	b.w.Settings.MinParticipationPercent = 50
	b.wants("h", rebootNeed(), packagesNeed("2.0"))

	out := b.step()

	// The reboot waits for approval, the packages for the budget: the host
	// is told about the budget.
	assert.Equal(t, "budget 8% of 10%", queueReasons(out.Plan)["h"])
	assert.Len(t, out.Plan.Queue[0].Rollouts, 2)
	assert.Empty(t, out.Plan.Queue[0].Rollout, "the budget is about no rollout")
}

// A host in two rollouts waits for one thing; the plan names the rollout
// that thing is about, so "approve" in a client cannot hit the other one.
func TestQueueEntryNamesTheRolloutItsReasonIsAbout(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyReboot = fleet.PolicyManual
	b.w.Settings.Paused = true
	b.host("h", 0)
	b.wants("h", packagesNeed("2.0"), rebootNeed())
	b.step()

	packages := b.mustRollout(packagesKey("2.0"))
	packages.Paused = true
	b.w.Settings.Paused = false

	var reboot *fleet.Rollout

	for _, r := range b.w.Rollouts {
		if r.Kind == fleet.RolloutReboot {
			reboot = r
		}
	}

	require.NotNil(t, reboot)

	out := b.step()

	require.Len(t, out.Plan.Queue, 1)

	entry := out.Plan.Queue[0]
	assert.Equal(t, WaitApproval, entry.Wait)
	assert.Equal(t, reboot.ID, entry.Rollout)
	assert.ElementsMatch(t, []string{packages.ID, reboot.ID}, entry.Rollouts)
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

	img := b.mustRollout(imageKey(imgLighthouse, digestA))
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

func TestAbortedCauseStaysClosed(t *testing.T) {
	tests := []struct {
		name      string
		closed    fleet.RolloutState
		needs     string
		wantsOpen bool
	}{
		{name: "aborted stays closed", closed: fleet.RolloutAborted, needs: digestA},
		{name: "the key changed", closed: fleet.RolloutAborted, needs: digestB, wantsOpen: true},
		{name: "completed reopens", closed: fleet.RolloutCompleted, needs: digestA, wantsOpen: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.host("a", 0)
			b.wantsImage("a", imgLighthouse, tt.needs)
			b.w.Closed = []*fleet.Rollout{
				{ID: "new", Kind: fleet.RolloutImage, Key: imageKey(imgLighthouse, digestA), Image: imgLighthouse, Digest: digestA, State: tt.closed},
				{ID: "old", Kind: fleet.RolloutImage, Key: imageKey(imgLighthouse, digestA), Image: imgLighthouse, Digest: digestA, State: fleet.RolloutCompleted},
			}

			out := b.step()

			assert.Equal(t, tt.wantsOpen, len(b.w.Rollouts) == 1)
			assert.Equal(t, tt.wantsOpen, b.lease("a") != nil)
			assert.Empty(t, out.Plan.Queue)
		})
	}
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

	first := b.mustRollout(imageKey(imgLighthouse, digestA))
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

	second := b.mustRollout(imageKey(imgLighthouse, digestB))
	assert.Equal(t, map[string]fleet.HostState{
		"h1": fleet.HostPending, "h2": fleet.HostExpected, "h3": fleet.HostExpected,
	}, memberStates(second), "nothing is granted before the hosts carried over had their turn")
	assert.Equal(t, carriedPrefix+first.ID, second.Host("h3").Error)
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
	assert.Empty(t, second.Canary, "no canary while a carried host is being repaired")
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
	r := b.mustRollout(imageKey(imgGeth, digestB))
	assert.Equal(t, map[string]fleet.HostState{
		"fresh-view": fleet.HostLeased, "stale-view": fleet.HostExpected,
	}, memberStates(r))

	// Stable: no rollout for the older digest appears on later passes.
	b.step()
	b.step()
	assert.Len(t, b.w.Rollouts, 1)
	assert.Empty(t, b.w.Closed)
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
	key := imageKey(imgLighthouse, digestA)

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
		custom.Custom = true
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
		b.w.Settings.PolicyImage = fleet.PolicyPaused
		b.w.Settings.PolicyPackages = fleet.PolicyPaused
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
		b.w.Settings.PolicyPackages = fleet.PolicyPaused
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

// TestCanaryThenTheRest: with no carried host, the canary alone, then
// everybody else at once. A bootnode is a host like any other.
func TestCanaryThenTheRest(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100

	b.host("tool1", 0)
	b.host("tool2", 0)
	b.host("val-heavy", 20)
	b.host("val-light", 5)
	b.host("val-mid", 10)
	b.host("boot1", 0, fleet.RoleBootnode)
	b.host("boot2", 8, fleet.RoleBootnode)

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
		b.host("boot", 3, fleet.RoleBootnode)
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

	t.Run("a carried host that waits for a person holds nobody back", func(t *testing.T) {
		for name, setup := range map[string]func(b *worldBuilder){
			"silent":  func(b *worldBuilder) { b.get("carried").LastSeen = b.w.Now.Add(-time.Hour) },
			"held":    func(b *worldBuilder) { b.get("carried").HoldWanted = true },
			"no verb": func(b *worldBuilder) { b.get("carried").Status.Verbs = []protocol.Verb{protocol.VerbRoll} },
		} {
			t.Run(name, func(t *testing.T) {
				b := newWorldBuilder(t)
				b.w.Settings.MaxUnavailablePercent = 100
				b.host("light", 5)
				b.host("mid", 8)
				b.host("carried", 30)

				for _, h := range b.w.Hosts {
					b.wants(h.Name, packagesNeed("2.0"))
				}

				b.w.Settings.Paused = true
				b.step()
				b.mustRollout(packagesKey("2.0")).Host("carried").CarriedFrom = "id000"
				b.w.Settings.Paused = false
				setup(b)

				out := b.step()
				r := b.mustRollout(packagesKey("2.0"))

				assert.Equal(t, []string{"light"}, b.leasedHosts())
				assert.Equal(t, "light", r.Canary)
				assert.Equal(t, fleet.StageRepair, r.Host("carried").Stage)
				assert.Equal(t, "after canary light", queueReasons(out.Plan)["mid"])
			})
		}
	})

	t.Run("a carried host that waits for the budget holds the others back", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 100
		b.host("a", 10)
		b.host("b", 10)
		b.host("tool", 0)

		for _, h := range b.w.Hosts {
			b.wants(h.Name, packagesNeed("2.0"))
		}

		// "a" attests, but was carried over as failed; the budget is zero.
		b.w.Settings.Paused = true
		b.step()
		b.mustRollout(packagesKey("2.0")).Host("a").CarriedFrom = "id000"
		b.w.Settings.Paused = false
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

func TestPolicyAndApproval(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyReboot = fleet.PolicyManual
	b.w.Settings.PolicyImage = fleet.PolicyPaused
	b.host("a", 0)
	b.wantsImage("a", imgLighthouse, digestA)
	b.wants("a", rebootNeed(), packagesNeed("2.0"))

	b.step()

	// Only the packages need (policy auto) is granted; the others stay
	// queued and do not hold the host back.
	l := b.lease("a")
	require.NotNil(t, l)
	require.Len(t, l.Needs, 1)
	assert.Equal(t, protocol.NeedPackages, l.Needs[0].Kind)
	assert.Equal(t, fleet.HostPending, b.mustRollout("reboot").Host("a").State)

	b.serve("a")
	b.get("a").Status.Operation = nil

	out := b.step()
	assert.Equal(t, "policy paused", queueReasons(out.Plan)["a"],
		"of approval (reboot) and policy paused (image), the later one in the list")

	// A person approves the reboot rollout.
	b.mustRollout("reboot").Approved = true
	b.step()

	l = b.lease("a")
	require.NotNil(t, l)
	require.Len(t, l.Needs, 1)
	assert.Equal(t, protocol.NeedReboot, l.Needs[0].Kind)
}

func TestGlobalPauseLetsOpenLeasesFinish(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.host("b", 0)
	b.wants("a", packagesNeed("2.0"))
	b.wants("b", packagesNeed("2.0"))
	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())

	b.w.Settings.Paused = true
	b.serve("a")

	out := b.step()
	assert.True(t, out.Plan.Paused)
	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, fleet.HostDone, b.mustRollout(packagesKey("2.0")).Host("a").State)
	assert.Equal(t, map[string]string{"b": "paused"}, queueReasons(out.Plan))
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

func TestStaleHostIsNotGranted(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.wants("a", packagesNeed("2.0"))
	b.get("a").LastSeen = b.w.Now.Add(-b.w.Settings.HostStaleAfter.Std() - time.Second)

	out := b.step()

	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, SlotStale, out.Plan.Hosts["a"].Slot)
	assert.Equal(t, "agent silent", out.Plan.Hosts["a"].Reason)

	b.get("a").LastSeen = b.w.Now.Add(-b.w.Settings.HostStaleAfter.Std())
	b.step()
	assert.Equal(t, []string{"a"}, b.leasedHosts())
}

func TestReconcileIsIdempotentAndPure(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 50

	for _, h := range []string{"t1", "t2", "v1", "v2", "v3"} {
		weight := uint64(0)
		if h[0] == 'v' {
			weight = 10
		}

		b.host(h, weight)
		b.wantsImage(h, imgLighthouse, digestA)
		b.wants(h, packagesNeed("2.0"))
	}

	b.get("t2").HoldWanted = true
	b.fillLiveness()

	first := Reconcile(b.w)
	require.NotEmpty(t, first.Leases)
	require.NotEmpty(t, first.Rollouts)
	assert.Empty(t, b.w.Rollouts, "the world is not touched")
	assert.Empty(t, b.w.Leases)
	assert.Empty(t, b.w.Directives)

	applyOutcome(b.w, first)

	second := Reconcile(b.w)
	assert.Empty(t, second.Rollouts)
	assert.Empty(t, second.Leases)
	assert.Empty(t, second.Directives)
	assert.Empty(t, second.Audit)
	assert.Equal(t, first.Plan, second.Plan)
}

// withoutLiveness makes the beacon view that of a node that does not serve
// the liveness endpoint.
func (b *worldBuilder) withoutLiveness() {
	b.healthMod = func(n *fleet.NetworkHealth) {
		n.LivenessKnown = false
		n.LivenessUnsupported = true
	}
	b.refreshHealth()
}

func TestBudgetWithoutLiveness(t *testing.T) {
	synced := protocol.NodeHealth{
		EL: &protocol.ELHealth{Reachable: true, Peers: 5},
		CL: &protocol.CLHealth{Reachable: true, Peers: 5},
	}
	syncing := protocol.NodeHealth{
		EL: &protocol.ELHealth{Reachable: true, Peers: 5},
		CL: &protocol.CLHealth{Reachable: true, Syncing: true, Peers: 5},
	}

	b := newWorldBuilder(t)
	b.withoutLiveness()
	b.host("synced", 10).Status.Node = synced
	b.host("syncing", 10).Status.Node = syncing
	b.host("silent", 10).Status.Node = synced
	b.host("leased", 10).Status.Node = synced
	b.host("noprobe", 7)
	b.host("tooling", 0).Status.Node = syncing
	b.get("silent").LastSeen = b.w.Now.Add(-b.w.Settings.HostStaleAfter.Std() - time.Second)
	b.w.Leases = []*fleet.Lease{{ID: "l1", Host: "leased", State: fleet.LeaseVerifying, AgentDoneAt: &b.w.Now}}

	// Rows from before the node stopped serving liveness are not looked at.
	for _, name := range []string{"synced", "syncing", "silent", "leased", "noprobe"} {
		b.live(b.w.Health.CurrentEpoch-1, name, 1)
	}

	plan := Reconcile(b.w).Plan

	assert.True(t, plan.Gate.OK)
	assert.True(t, plan.Gate.Network.LivenessUnsupported, "the plan says what the budget is built on")
	assert.Equal(t, Budget{
		TotalWeight:       47,
		AllowedWeight:     4.7,
		UnavailableWeight: 30, // "syncing" (probes), "silent" (stale) and "leased"
		LeasedHosts:       1,
		MaxLeasedHosts:    1,
	}, plan.Budget)
	assert.Equal(t, HostPlan{Slot: SlotIdle, Weight: 10, Live: 10}, plan.Hosts["synced"])
	assert.Equal(t, HostPlan{Slot: SlotIdle, Weight: 10, Live: 0}, plan.Hosts["syncing"])
	assert.Equal(t, HostPlan{Slot: SlotStale, Weight: 10, Live: 0}, plan.Hosts["silent"])
	assert.Equal(t, HostPlan{Slot: SlotIdle, Weight: 7, Live: 7}, plan.Hosts["noprobe"],
		"no probe configured: nothing says it is down")
}

func TestGrantsWithoutLiveness(t *testing.T) {
	synced := protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Peers: 5}}
	unreachable := protocol.NodeHealth{CL: &protocol.CLHealth{}}

	t.Run("a fleet with validators rolls, verified by the probes", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.withoutLiveness()

		for _, name := range []string{"a", "b", "c"} {
			b.host(name, 10).Status.Node = synced
			b.wants(name, packagesNeed("2.0"))
		}

		out := b.step()
		require.True(t, out.Plan.Gate.OK)
		require.Equal(t, []string{"a"}, b.leasedHosts(), "the canary")
		assert.Equal(t, uint64(10), b.lease("a").Charged)

		b.finish("a")
		b.step()
		b.advance(5 * time.Second)
		b.step()
		assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", b.lease("a").Readiness)

		b.advance(testEpoch)
		b.step()
		require.Equal(t, []string{"b"}, b.leasedHosts(), "released by the probe rule, the next host follows")

		b.serve("b")
		b.serve("c")
		assert.Equal(t, fleet.RolloutCompleted, b.closed(packagesKey("2.0")).State)
	})

	t.Run("a host whose probes are not synced is down already: free, and it uses up the budget", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.withoutLiveness()
		b.host("down", 20).Status.Node = unreachable
		b.host("up", 20).Status.Node = synced
		b.host("idle", 60).Status.Node = synced
		b.wants("down", packagesNeed("2.0"))
		b.wants("up", packagesNeed("2.0"))
		b.wants("up", rebootNeed())

		out := b.step()

		// 20 of 100 are down, twice the 10 allowed.
		assert.Equal(t, []string{"down"}, b.leasedHosts())
		assert.Equal(t, uint64(0), b.lease("down").Charged)
		assert.Equal(t, uint64(20), out.Plan.Budget.UnavailableWeight)
		assert.Equal(t, "budget 20% of 10%", queueReasons(out.Plan)["up"])
	})

	t.Run("liveness that is merely unknown keeps the gate closed", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
		b.refreshHealth()
		b.host("a", 10).Status.Node = synced
		b.wants("a", packagesNeed("2.0"))

		out := b.step()

		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, map[string]string{"a": "network gate: liveness data stale"}, queueReasons(out.Plan))
	})

	t.Run("readiness uses the probes even when old liveness rows are around", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("a", 10).Status.Node = synced
		b.wants("a", packagesNeed("2.0"))
		b.step()
		require.Equal(t, []string{"a"}, b.leasedHosts())

		// The node stops serving liveness while the lease is open; a view that
		// still claimed to know liveness must not bring the epoch rule back.
		b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessUnsupported = true }
		b.refreshHealth()
		b.finish("a")
		b.get("a").Status.Node = unreachable
		b.step()
		b.advance(5 * time.Second)
		b.step()

		assert.Equal(t, "CL is not reachable", b.lease("a").Readiness)
	})
}

// A host that a rollout broke keeps participation below the gate. The gate
// must not keep its repair away, or nothing would ever recover.
func TestFailingGatePassesTheRepairOfADownHost(t *testing.T) {
	setup := func(t *testing.T) *worldBuilder {
		t.Helper()

		b := newWorldBuilder(t)
		for _, name := range []string{"a", "b", "c", "d"} {
			b.host(name, 32)
			b.wants(name, packagesNeed("2.0"))
		}

		b.fillLiveness()
		// d has attested with nothing for two epochs: 96 of 128 live, 75% < 90%.
		b.down("d")
		b.refreshHealth()

		return b
	}

	t.Run("participation below the floor", func(t *testing.T) {
		b := setup(t)
		out := b.step()

		require.False(t, out.Plan.Gate.OK)
		require.Equal(t, []string{"d"}, b.leasedHosts(), "the down host is repaired, nobody else moves")
		assert.Zero(t, b.lease("d").Charged)
		assert.True(t, b.lease("d").Repair)
		// It was the canary already: with a quarter of the weight down it is
		// the only member that fits the budget.
		assert.Equal(t, "d", b.mustRollout(packagesKey("2.0")).Canary)
		assert.Equal(t, fleet.StageCanary, b.mustRollout(packagesKey("2.0")).Host("d").Stage)

		reasons := queueReasons(out.Plan)
		for _, name := range []string{"a", "b", "c"} {
			assert.Contains(t, reasons[name], "network gate: participation", name)
		}
	})

	t.Run("finality lag too", func(t *testing.T) {
		b := setup(t)
		b.w.Health.FinalityLagEpochs = 9
		b.step()

		require.Equal(t, []string{"d"}, b.leasedHosts())
	})

	t.Run("not without data to trust", func(t *testing.T) {
		b := setup(t)
		b.w.Health.UpdatedAt = b.w.Now.Add(-time.Hour)
		out := b.step()

		require.Empty(t, b.leasedHosts())
		assert.Contains(t, queueReasons(out.Plan)["d"], "network gate: "+gateBeacon)
	})

	t.Run("a host without validators is not a repair", func(t *testing.T) {
		b := setup(t)
		b.host("plain", 0)
		b.wants("plain", packagesNeed("2.0"))
		// Take the down host out of the picture.
		b.get("d").HoldWanted = true
		out := b.step()

		assert.NotContains(t, b.leasedHosts(), "plain")
		assert.Contains(t, queueReasons(out.Plan)["plain"], "network gate: participation")
	})
}
