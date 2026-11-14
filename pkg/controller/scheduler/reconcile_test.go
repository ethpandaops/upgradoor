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
	assert.Equal(t, "after canary down", queueReasons(out.Plan)["up"])

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

	out := b.step()
	assert.Equal(t, []string{"tool1"}, b.leasedHosts())
	assert.Equal(t, "host limit 1 of 1", queueReasons(out.Plan)["tool2"])

	b.serve("tool1")
	b.serve("tool2")

	out = b.step()
	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, map[string]string{"val": "budget 0% of 0%"}, queueReasons(out.Plan))
}

func TestHostLimitBoundsHostsWithoutValidators(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 25

	for _, h := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8"} {
		b.host(h, 0)
		b.wants(h, packagesNeed("2.0"))
	}

	out := b.step()
	assert.Equal(t, []string{"t1", "t2"}, b.leasedHosts())
	assert.Equal(t, 2, out.Plan.Budget.MaxLeasedHosts)
	assert.Equal(t, "host limit 2 of 2", queueReasons(out.Plan)["t3"])
	assert.Equal(t, 1, out.Plan.Queue[0].Position)
	assert.Equal(t, "t3", out.Plan.Queue[0].Host)
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
				b.host("first", 0)
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
			name: "after hosts without validators",
			setup: func(b *worldBuilder) {
				b.host("tool", 0)
				b.wants("tool", packagesNeed("2.0"))
			},
			want: "after hosts without validators",
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
				b.host("first", 0)
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
				b.host("first", 0)
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
				b.host("tool", 0)
				b.wants("tool", packagesNeed("2.0"))
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

	b.step()
	second = b.mustRollout(second.Key)
	assert.Equal(t, []string{"h3"}, b.leasedHosts())
	assert.Equal(t, "h3", second.Canary)
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

	t.Run("an expected host blocks later stages only for hostStaleAfter", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("tool", 0)
		b.host("val", 10)
		b.wantsImage("val", imgLighthouse, digestA)
		b.get("tool").Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestOld, b.w.Now)}

		out := b.step()
		assert.Equal(t, "after hosts without validators", queueReasons(out.Plan)["val"])

		b.advance(b.w.Settings.HostStaleAfter.Std() + time.Second)
		b.step()
		assert.Equal(t, []string{"val"}, b.leasedHosts())
		assert.Equal(t, fleet.HostExpected, b.mustRollout(key).Host("tool").State, "it is still shown as expected")

		// The rollout completes without it.
		b.serve("val")
		assert.Equal(t, fleet.RolloutCompleted, b.closed(key).State)
	})

	t.Run("an expected host that is held, stale or lacks the verb does not block", func(t *testing.T) {
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
				b.wantsImage("val", imgLighthouse, digestA)
				b.get("tool").Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestOld, b.w.Now)}
				tt.setup(b.get("tool"), b.w.Now)

				b.step()

				assert.Equal(t, []string{"val"}, b.leasedHosts())
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

func TestStageOrdering(t *testing.T) {
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

	// Stage 0: hosts without validators, several at once.
	out := b.step()
	assert.Equal(t, []string{"tool1", "tool2"}, b.leasedHosts())
	assert.Equal(t, map[string]string{
		"val-light": "after hosts without validators",
		"val-mid":   "after hosts without validators",
		"val-heavy": "after hosts without validators",
		"boot1":     "after hosts without validators",
		"boot2":     "after hosts without validators",
	}, queueReasons(out.Plan))

	order := make([]string, 0, len(out.Plan.Queue))
	for _, q := range out.Plan.Queue {
		order = append(order, q.Host)
	}

	assert.Equal(t, []string{"val-light", "val-mid", "val-heavy", "boot1", "boot2"}, order,
		"validator hosts by ascending weight, bootnodes last")

	b.serve("tool1")
	assert.Equal(t, []string{"tool2"}, b.leasedHosts(), "stage 0 is not settled yet")
	b.serve("tool2")

	// Stage 1: the canary is the lightest validator host, alone.
	out = b.step()
	assert.Equal(t, []string{"val-light"}, b.leasedHosts())
	assert.Equal(t, "val-light", b.mustRollout(key).Canary)
	assert.Equal(t, fleet.StageCanary, b.mustRollout(key).Host("val-light").Stage)
	assert.Equal(t, map[string]string{
		"val-mid":   "after canary val-light",
		"val-heavy": "after canary val-light",
		"boot1":     "after validator hosts",
		"boot2":     "after validator hosts",
	}, queueReasons(out.Plan))

	// Stage 2: the rest of the validator hosts together.
	b.serve("val-light")
	assert.Equal(t, []string{"val-heavy", "val-mid"}, b.leasedHosts())
	assert.Equal(t, fleet.StageBatches, b.mustRollout(key).Host("val-mid").Stage)

	b.serve("val-mid")
	assert.Equal(t, []string{"val-heavy"}, b.leasedHosts())

	// Stage 3: bootnodes, one at a time, also the one with validators.
	b.serve("val-heavy")

	out = b.step()
	assert.Equal(t, []string{"boot1"}, b.leasedHosts())
	assert.Equal(t, map[string]string{"boot2": "after bootnode boot1"}, queueReasons(out.Plan))
	assert.Equal(t, fleet.StageBootnodes, b.mustRollout(key).Host("boot2").Stage)

	b.serve("boot1")
	assert.Equal(t, []string{"boot2"}, b.leasedHosts())
	b.serve("boot2")
	assert.Equal(t, fleet.RolloutCompleted, b.closed(key).State)
}

func TestCanaryCandidates(t *testing.T) {
	t.Run("a host that is fully down goes first", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MinParticipationPercent = 10
		b.host("light", 5)
		b.host("down", 30)
		b.wants("light", packagesNeed("2.0"))
		b.wants("down", packagesNeed("2.0"))
		b.live(b.w.Health.LivenessEpoch, "down", 0)

		b.step()

		assert.Equal(t, []string{"down"}, b.leasedHosts())
		assert.Equal(t, "down", b.mustRollout(packagesKey("2.0")).Canary)
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
		// one like every other validator host.
		b.wants("a", packagesNeed("2.0"))

		out := b.step()

		assert.Equal(t, []string{"b"}, b.leasedHosts())
		assert.Equal(t, "after canary b", queueReasons(out.Plan)["a"])
		assert.Equal(t, fleet.StageBatches, b.mustRollout(packagesKey("2.0")).Host("a").Stage)
	})

	t.Run("a failed canary halts and nobody follows", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.MaxUnavailablePercent = 100
		b.host("a", 5)
		b.host("b", 6)
		b.wants("a", packagesNeed("2.0"))
		b.wants("b", packagesNeed("2.0"))
		b.step()
		b.report("a", protocol.PhaseFailed, true)

		out := b.step()

		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, "rollout halted: a: container did not start", queueReasons(out.Plan)["b"])
		assert.Equal(t, SlotFailed, out.Plan.Hosts["a"].Slot)
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
		assert.Equal(t, "probes synced, soaking for 6m24s", b.lease("a").Readiness)

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

		assert.Equal(t, "probes do not report synced", b.lease("a").Readiness)
	})
}
