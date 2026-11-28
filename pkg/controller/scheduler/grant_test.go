package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The one exception of the network gate: while the gate is closed for
// participation or finality, a host none of whose validators attested in the
// newest epoch is granted when nothing at all is leased. It does not wait for
// its place in the rollout, and nobody is granted next to it.
func TestClosedGatePassesOneHostWithoutLiveValidators(t *testing.T) {
	const closed = "network gate: participation 80% < 90%"

	tests := []struct {
		name  string
		setup func(b *worldBuilder)
		want  []string
		// reason is why the canary, h0, waits.
		reason string
	}{
		{
			name: "participation below the floor: one dead host passes, the other waits",
			setup: func(b *worldBuilder) {
				b.down("h4")
				b.down("h8")
			},
			want:   []string{"h4"},
			reason: closed,
		},
		{
			name: "finality lags",
			setup: func(b *worldBuilder) {
				b.down("h4")
				b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 5 }
				b.refreshHealth()
			},
			want:   []string{"h4"},
			reason: "network gate: finality lag 5 epochs > 3",
		},
		{
			name: "a gate that lacks liveness data passes nobody",
			setup: func(b *worldBuilder) {
				b.down("h4")
				b.down("h8")
				b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
				b.refreshHealth()
			},
			want:   []string{},
			reason: "network gate: attestation data missing or stale",
		},
		{
			name: "a gate whose beacon view is stale passes nobody",
			setup: func(b *worldBuilder) {
				b.down("h4")
				b.down("h8")
				b.healthMod = func(n *fleet.NetworkHealth) { n.UpdatedAt = n.UpdatedAt.Add(-3 * testEpoch) }
				b.refreshHealth()
			},
			want:   []string{},
			reason: "network gate: beacon node unreachable",
		},
		{
			name: "nobody passes while a lease is open",
			setup: func(b *worldBuilder) {
				// The canary goes while the gate is open. Then two hosts die:
				// 70 of the 90 that hold no lease attest. The host limit of 2
				// would let a second host through.
				b.w.Settings.MaxUnavailablePercent = 20
				b.step()
				b.down("h4")
				b.down("h8")
			},
			want:   []string{"h0"},
			reason: "",
		},
		{
			name: "a host with one validator attesting does not pass",
			setup: func(b *worldBuilder) {
				b.liveOverride["h4"] = 1
				b.liveOverride["h8"] = 1
			},
			want:   []string{},
			reason: "network gate: participation 82% < 90%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)

			for i := range 10 {
				name := fmt.Sprintf("h%d", i)
				b.host(name, 10)
				b.wants(name, packagesNeed("2.0"))
			}

			b.host("boot", 0)
			b.wants("boot", packagesNeed("2.0"))
			tt.setup(b)

			out := b.step()
			assert.Equal(t, tt.want, b.leasedHosts())
			assert.Equal(t, tt.reason, queueReasons(out.Plan)["h0"])

			// It stays so: the host that passed is left out of the gate, which is
			// closed for the others all the same, and nothing is leased next to it.
			b.advance(time.Minute)
			b.step()
			assert.Equal(t, tt.want, b.leasedHosts())
		})
	}
}

// Nothing rolls through a closed gate by itself. A rollout a person approved
// passes it one host at a time, in its order, whatever the budget would allow.
func TestClosedGatePassesAnApprovedRolloutOneHostAtATime(t *testing.T) {
	const closed = "network gate: finality lag 5 epochs > 3"

	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.MaxLeasedPercent = 100

	for _, h := range []string{"a", "b", "c"} {
		b.host(h, 10)
		b.wants(h, packagesNeed("2.0"))
	}

	b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 5 }
	b.refreshHealth()

	out := b.step()
	assert.Empty(t, b.leasedHosts(), "an auto rollout that nobody approved waits")
	assert.Equal(t, map[string]string{"a": closed, "b": closed, "c": closed}, queueReasons(out.Plan))
	assert.Equal(t, WaitGate, out.Plan.Queue[0].Wait)

	r := b.mustRollout(packagesKey("2.0"))
	r.Approved = true

	out = b.step()
	assert.Equal(t, []string{"a"}, b.leasedHosts(), "the canary")
	assert.Equal(t, map[string]string{"b": closed, "c": closed}, queueReasons(out.Plan))

	b.serve("a")
	assert.Len(t, b.leasedHosts(), 1, "one host at a time after the canary too")
}

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
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 5, leased: 1, weighted: 1, maxLeased: 3},
			cost:   6,
			want:   "validators down: 5% now, 11% with this host, limit 10%",
		},
		{
			name:   "floor: heavy host goes when nothing is unavailable",
			budget: budgetState{total: 100, percent: 10, allowed: 10, maxLeased: 1},
			cost:   40,
		},
		{
			name:   "floor: heavy host goes while a host without validators holds a lease",
			budget: budgetState{total: 100, percent: 10, allowed: 10, leased: 1, maxLeased: 3},
			cost:   40,
		},
		{
			name:   "floor: heavy host goes while validators elsewhere miss their duties",
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 33, maxLeased: 3},
			cost:   40,
		},
		{
			name:   "no floor while a host with validators holds a lease",
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 1, leased: 1, weighted: 1, maxLeased: 3},
			cost:   40,
			want:   "validators down: 1% now, 41% with this host, limit 10%",
		},
		{
			name:   "the floor does not lift the host limit",
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 33, leased: 1, maxLeased: 1},
			cost:   40,
			want:   "hosts at once: 1 of 1 busy",
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
			want:   "validators down: 0% now, 1% with this host, limit 0%",
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
			want:   "hosts at once: 2 of 2 busy",
		},
		{
			name:   "budget is named before the host limit",
			budget: budgetState{total: 100, percent: 10, allowed: 10, unavailable: 10, leased: 2, weighted: 2, maxLeased: 2},
			cost:   1,
			want:   "validators down: 10% now, 11% with this host, limit 10%",
		},
		{
			name:   "fractional percent",
			budget: budgetState{total: 8, percent: 12.5, allowed: 1, leased: 0, maxLeased: 1},
			cost:   1,
		},
		{
			name:   "percent is rounded down in the reason",
			budget: budgetState{total: 3, percent: 12.5, allowed: 0.375, unavailable: 1, leased: 1, weighted: 1, maxLeased: 2},
			cost:   1,
			want:   "validators down: 33.3% now, 66.7% with this host, limit 12.5%",
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
		MaxIndependent:    1,
	}, plan.Budget)
	assert.Equal(t, HostPlan{Slot: SlotIdle, Weight: 7, Live: 7}, plan.Hosts["norow"])
	assert.Equal(t, HostPlan{
		Slot: SlotIdle, Weight: 10, Live: 4, LiveKnown: true, NotReady: "validators: 4 of 10 attesting",
	}, plan.Hosts["partial"])
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
	assert.Equal(t, map[string]string{"big": "validators down: 39% now, 99% with this host, limit 10%"}, queueReasons(out.Plan))

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
	b.wantsKind("down", protocol.KindVC, imgLighthouse, digestA)
	b.wantsKind("up", protocol.KindVC, imgLighthouse, digestA)

	out := b.step()

	// 20 of 100 are down already, twice the 10 allowed. Rolling the host
	// that is down costs nothing more; its validator client is what it is
	// not ready for.
	assert.Equal(t, []string{"down"}, b.leasedHosts())
	assert.Equal(t, uint64(0), b.lease("down").Charged)
	assert.Equal(t, uint64(20), b.lease("down").Weight)
	assert.Equal(t, uint64(20), out.Plan.Budget.UnavailableWeight)
	assert.Equal(t, "after canary down", queueReasons(out.Plan)["up"],
		"the only host that fits the budget is the canary")

	// A second cause shows the budget for the host that is up.
	b.wants("up", rebootNeed())

	out = b.step()
	assert.Equal(t, "validators down: 20% now, 40% with this host, limit 10%", queueReasons(out.Plan)["up"])
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
	assert.Equal(t, map[string]string{"val": "validators down: 0% now, 100% with this host, limit 0%"}, queueReasons(out.Plan))
}

// TestHostsThatCostNothingPassAFullBudget: in the rest, hosts without
// validators are served first; the host with validators would have the floor
// lease, but the host limit is full.
func TestHostsThatCostNothingPassAFullBudget(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 50
	b.w.Settings.MaxLeasedPercent = 50
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
	assert.Equal(t, map[string]string{"heavy": "hosts at once: 2 of 2 busy"}, queueReasons(out.Plan))
}

func TestHostLimitBoundsHostsWithoutValidators(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxLeasedPercent = 25

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
	assert.Equal(t, "hosts at once: 2 of 2 busy", queueReasons(out.Plan)["t4"])
	assert.Equal(t, 1, out.Plan.Queue[0].Position)
	assert.Equal(t, "t4", out.Plan.Queue[0].Host)
	assert.Equal(t, WaitHostLimit, out.Plan.Queue[0].Wait)
}

// The host limit and the validator budget are separate settings: either one
// alone stops the rest of a rollout.
func TestHostLimitAndBudgetAreIndependent(t *testing.T) {
	for _, tt := range []struct {
		name        string
		unavailable float64
		leased      float64
		wantLeased  []string
		wantMax     int
		wantReason  string
	}{
		{
			name: "host limit", unavailable: 100, leased: 25,
			wantLeased: []string{"h2", "h3"}, wantMax: 2, wantReason: "hosts at once: 2 of 2 busy",
		},
		{
			name: "budget", unavailable: 25, leased: 100,
			wantLeased: []string{"h2", "h3"}, wantMax: 8, wantReason: "validators down: 25% now, 37.5% with this host, limit 25%",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.w.Settings.MaxUnavailablePercent = tt.unavailable
			b.w.Settings.MaxLeasedPercent = tt.leased

			for _, h := range []string{"h1", "h2", "h3", "h4", "h5", "h6", "h7", "h8"} {
				b.host(h, 10)
				b.wants(h, packagesNeed("2.0"))
			}

			b.step()
			require.Equal(t, []string{"h1"}, b.leasedHosts(), "the canary")
			b.serve("h1")

			out := b.step()
			assert.Equal(t, tt.wantLeased, b.leasedHosts())
			assert.Equal(t, tt.wantMax, out.Plan.Budget.MaxLeasedHosts)
			assert.Equal(t, tt.wantReason, queueReasons(out.Plan)["h4"])
		})
	}
}

// Every reason a host can wait for has a wait kind: a client that sorts the
// queue into "starts by itself" and "needs somebody" must never see a blank.
func TestEveryWaitingRankHasAKind(t *testing.T) {
	seen := make(map[WaitKind]bool, len(waitKinds))

	for rank := rankHeld; rank <= rankHostLimit; rank++ {
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
			want:  []string{"beacon node unreachable", "attestation data missing or stale"},
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
			name: "network split",
			setup: func(b *worldBuilder) {
				b.w.Health.Split = "2 chains, 2/4 and 2/4 endpoints"
			},
			want: []string{"network split: 2 chains, 2/4 and 2/4 endpoints"},
		},
		{
			name: "the node answers and its chain timing is invalid",
			setup: func(b *worldBuilder) {
				b.w.Health.Phases[0].SlotMs = 0
				b.w.Health.TimingError = "phase 0 (from epoch 0) has slots of no duration"
			},
			want: []string{"invalid chain timing: phase 0 (from epoch 0) has slots of no duration"},
		},
		{
			name: "a node that stopped answering is unreachable, whatever its timing was",
			setup: func(b *worldBuilder) {
				b.w.Health.Phases = nil
				b.w.Health.TimingError = "no phases"
				b.w.Health.UpdatedAt = b.w.Now.Add(-2*testEpoch - time.Second)
			},
			want: []string{"beacon node unreachable"},
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
			name: "a host that holds a lease is left out on both sides",
			setup: func(b *worldBuilder) {
				// "a" is down under a lease: 50 of the 50 remaining attest.
				b.live(b.w.Health.LivenessEpoch, "a", 0)
				b.w.Leases = []*fleet.Lease{{ID: "l1", Host: "a", State: fleet.LeaseRunning, Weight: 50, Charged: 30}}
				b.get("a").Status.Operation = &protocol.Operation{LeaseID: "l1", Phase: protocol.PhaseApplying}
			},
			want: []string{},
		},
		{
			name: "a host that works without a lease is left out too",
			setup: func(b *worldBuilder) {
				b.live(b.w.Health.LivenessEpoch, "a", 0)
				b.get("a").Status.Operation = &protocol.Operation{LeaseID: "gone", Phase: protocol.PhaseApplying}
			},
			want: []string{},
		},
		{
			name: "liveness unknown with tracked validators",
			setup: func(b *worldBuilder) {
				b.w.Health.LivenessKnown = false
			},
			want: []string{"attestation data missing or stale"},
		},
		{
			name: "the beacon service says why the liveness is missing",
			setup: func(b *worldBuilder) {
				b.w.Health.LivenessKnown = false
				b.w.Health.LivenessError = "reading committees of epoch 9: HTTP 500"
			},
			want: []string{"attestation data missing or stale: reading committees of epoch 9: HTTP 500"},
		},
		{
			name: "liveness more than two epochs behind",
			setup: func(b *worldBuilder) {
				b.w.Health.CurrentEpoch += 2
			},
			want: []string{"attestation data missing or stale"},
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
			name: "held",
			setup: func(b *worldBuilder) {
				b.get("h").Status.Hold.Holds = []protocol.HostHold{{Holder: fleet.HolderStaff}, {Holder: "geth"}}
			},
			want: "held by staff, geth",
		},
		{
			name: "hold requested",
			setup: func(b *worldBuilder) {
				b.w.Directives = append(b.w.Directives, &fleet.Directive{
					Directive: protocol.Directive{ID: "d-hold", Kind: protocol.DirectiveHold, Holder: "geth"},
					Host:      "h", State: fleet.DirectivePending,
				})
			},
			want: "hold requested, waiting for the host",
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
				b.w.Settings.PolicyPackages = fleet.PolicyManual
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
				b.host("sick", 80)
				b.live(b.w.Health.LivenessEpoch, "sick", 72) // 8 of 100 down
				b.host("busy", 10)
				b.held("busy") // no floor lease while it works
				b.w.Settings.MinParticipationPercent = 50
			},
			want: "validators down: 18% now, 28% with this host, limit 10%",
		},
		{
			name: "held wins over everything else",
			setup: func(b *worldBuilder) {
				b.get("h").Status.Hold = protocol.Hold{Local: true, Holds: []protocol.HostHold{{Holder: protocol.HolderHost}}}
				b.get("h").LastSeen = b.w.Now.Add(-3 * time.Minute)
				b.w.Settings.Paused = true
				b.w.Settings.PolicyPackages = fleet.PolicyManual
				b.w.Health.FinalityLagEpochs = 5
			},
			want: "held by the host",
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
			name: "waiting for approval wins over halted",
			setup: func(b *worldBuilder) {
				b.host("first", 5)
				b.wants("first", packagesNeed("2.0"))
				b.step()
				b.report("first", protocol.PhaseFailed, true)
				b.w.Settings.PolicyPackages = fleet.PolicyManual
			},
			want: "waiting for approval (policy manual)",
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
		{
			name: "failed pull",
			setup: func(b *worldBuilder) {
				b.get("h").Status.Needs[0].PullError = "manifest unknown"
			},
			want: "waiting to pull its image again after a failed pull",
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
	b.host("other", 80)
	b.host("busy", 10)
	b.host("h", 10)
	b.live(b.w.Health.LivenessEpoch, "other", 72)
	b.held("busy")
	b.w.Settings.MinParticipationPercent = 50
	b.wants("h", rebootNeed(), packagesNeed("2.0"))

	out := b.step()

	// The reboot waits for approval, the packages for the budget: the host
	// is told about the budget, and about the rollout that waits for it.
	assert.Equal(t, "validators down: 18% now, 28% with this host, limit 10%", queueReasons(out.Plan)["h"])
	require.Len(t, out.Plan.Queue, 1)
	assert.Len(t, out.Plan.Queue[0].Rollouts, 2)
	assert.Equal(t, WaitBudget, out.Plan.Queue[0].Wait)
	assert.Equal(t, b.mustRollout(packagesKey("2.0")).ID, out.Plan.Queue[0].Rollout)
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

func TestPolicyAndApproval(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyReboot = fleet.PolicyManual
	b.w.Settings.PolicyImage = fleet.PolicyManual
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
	assert.Equal(t, "waiting for approval (policy manual)", queueReasons(out.Plan)["a"])

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

// An agent the controller does not serve waits like a silent one, with the
// protocol as its reason.
func TestIncompatibleHostIsNotGranted(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("a", 0)
	b.wants("a", packagesNeed("2.0"))
	b.get("a").Status.ProtocolVersion = protocol.Version + 1

	out := b.step()

	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, SlotStale, out.Plan.Hosts["a"].Slot)
	assert.Equal(t, fleet.Unsupported(protocol.Version+1), out.Plan.Hosts["a"].Reason)

	b.get("a").Status.ProtocolVersion = protocol.Version
	b.step()
	assert.Equal(t, []string{"a"}, b.leasedHosts())
}

func TestUnknownLivenessKeepsTheGateClosed(t *testing.T) {
	synced := protocol.NodeHealth{CL: &protocol.CLHealth{Reachable: true, Peers: 5}}

	b := newWorldBuilder(t)
	b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
	b.refreshHealth()
	b.host("a", 10).Status.Node = synced
	b.wants("a", packagesNeed("2.0"))

	out := b.step()

	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, map[string]string{"a": "network gate: attestation data missing or stale"}, queueReasons(out.Plan))
}

func TestForceRollout(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.MaxLeasedPercent = 100
	b.host("boot", 5, "bootnode")
	b.host("val", 10)
	b.host("tool", 0)
	b.manualRollout("m1", true, false, "boot", "val", "tool")

	// No canary and no order: everybody the budget lets through, at once.
	out := b.step()
	assert.Equal(t, []string{"boot", "tool", "val"}, b.leasedHosts())
	assert.Empty(t, out.Plan.Queue)
	assert.Empty(t, b.mustRollout("manual:m1").Canary)

	l := b.lease("tool")
	assert.True(t, l.Force)
	assert.Equal(t, "alice", l.GrantedBy)

	// Released as soon as the agent reports done: no readiness check.
	b.finish("val")

	out = b.step()
	assert.Nil(t, b.lease("val"))
	assert.Contains(t, auditActions(out), "lease.released")
	assert.Equal(t, fleet.HostDone, b.mustRollout("manual:m1").Host("val").State)
}

func TestForceStillAnswersToBudgetAndGate(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("v1", 50)
	b.host("v2", 50)
	b.manualRollout("m1", true, false, "v1", "v2")

	out := b.step()
	assert.Equal(t, []string{"v1"}, b.leasedHosts())
	assert.Equal(t, "validators down: 50% now, 100% with this host, limit 10%", queueReasons(out.Plan)["v2"])

	b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 9 }
	b.refreshHealth()

	out = b.step()
	assert.Equal(t, "network gate: finality lag 9 epochs > 3", queueReasons(out.Plan)["v2"])
}

func TestIgnoreBudget(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 0
	b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 9 }
	b.refreshHealth()
	b.host("v1", 50)
	b.host("v2", 50)
	b.manualRollout("m1", true, true, "v1", "v2")
	// An observed cause on the same host does not ride along past the gate.
	b.wants("v1", packagesNeed("2.0"))

	b.step()
	b.step()

	assert.Equal(t, []string{"v1", "v2"}, b.leasedHosts(), "budget, host limit and gate are skipped")
	require.Len(t, b.lease("v1").Needs, 1)
	assert.Equal(t, protocol.NeedReboot, b.lease("v1").Needs[0].Kind)
	assert.Equal(t, fleet.HostPending, b.mustRollout(packagesKey("2.0")).Host("v1").State)

	t.Run("the global pause still applies", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.w.Settings.Paused = true
		b.host("v1", 50)
		b.manualRollout("m1", true, true, "v1")

		out := b.step()

		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, "paused", queueReasons(out.Plan)["v1"])
	})
}

// A rollout a person let pass the gate passes it for any reason, missing
// data included; the order and the budget still apply, and the other
// rollouts keep waiting at the gate.
func TestIgnoreGate(t *testing.T) {
	for name, mod := range map[string]func(n *fleet.NetworkHealth){
		"finality":         func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 9 },
		"beacon node down": func(n *fleet.NetworkHealth) { n.UpdatedAt = time.Time{} },
		"no liveness":      func(n *fleet.NetworkHealth) { n.LivenessKnown = false },
	} {
		t.Run(name, func(t *testing.T) {
			b := newWorldBuilder(t)
			b.healthMod = mod
			b.refreshHealth()
			b.host("v1", 10)
			b.host("v2", 20)
			b.host("v3", 30)
			b.manualRollout("m1", false, false, "v1", "v2").IgnoreGate = true
			b.wants("v3", packagesNeed("2.0"))

			out := b.step()

			assert.Equal(t, []string{"v1"}, b.leasedHosts())
			assert.Equal(t, "after canary v1", queueReasons(out.Plan)["v2"])
			assert.True(t, strings.HasPrefix(queueReasons(out.Plan)["v3"], ReasonGatePrefix), queueReasons(out.Plan)["v3"])

			granted := slices.IndexFunc(out.Audit, func(e fleet.AuditEntry) bool { return e.Action == "lease.granted" })
			require.GreaterOrEqual(t, granted, 0)
			assert.Equal(t, true, out.Audit[granted].Detail["ignoredGate"])
		})
	}

	t.Run("the budget still applies", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 9 }
		b.refreshHealth()
		b.host("v1", 50)
		b.host("v2", 50)
		b.manualRollout("m1", true, false, "v1", "v2").IgnoreGate = true

		out := b.step()

		assert.Equal(t, []string{"v1"}, b.leasedHosts())
		assert.Equal(t, "validators down: 50% now, 100% with this host, limit 10%", queueReasons(out.Plan)["v2"])
	})
}

// wantsImageOf is wantsImage for a container of an impact class.
func (b *worldBuilder) wantsImageOf(host, image, to string, impact protocol.Impact) {
	b.wantsImage(host, image, to)

	cs := b.get(host).Status.Containers
	cs[len(cs)-1].Impact = impact
}

// Independent work has a limit of its own and no gate; it neither waits for
// node work nor holds it back.
func TestIndependentWorkNeitherWaitsNorBlocks(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxIndependentPercent = 25
	b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 5 }
	b.refreshHealth()

	for _, h := range []string{"n1", "n2", "n3", "n4"} {
		b.host(h, 10)
		b.wantsImageOf(h, imgGeth, digestA, protocol.ImpactNode)
	}

	for _, h := range []string{"x1", "x2", "x3", "x4"} {
		b.host(h, 10)
		b.wantsImageOf(h, "org/exporter:latest", digestB, protocol.ImpactIndependent)
	}

	out := b.step()
	require.Equal(t, []string{"x1"}, b.leasedHosts(), "the independent canary passes the closed gate")
	assert.Equal(t, protocol.ImpactIndependent, b.lease("x1").Impact)
	assert.Zero(t, b.lease("x1").Charged)
	assert.Equal(t, "network gate: finality lag 5 epochs > 3", queueReasons(out.Plan)["n1"])

	b.finish("x1")
	b.step()
	b.advance(5 * time.Second)
	b.step()
	require.Nil(t, b.lease("x1"), "independent work is ready at the first sync after done")

	out = b.step()
	assert.Equal(t, []string{"x2", "x3"}, b.leasedHosts())
	assert.Equal(t, "independent hosts at once: 2 of 2 busy", queueReasons(out.Plan)["x4"])

	b.healthMod = nil
	b.refreshHealth()

	out = b.step()
	assert.Equal(t, []string{"n1", "x2", "x3"}, b.leasedHosts(), "node work is not held back by independent leases")
	assert.Equal(t, 1, out.Plan.Budget.LeasedHosts)
	assert.Equal(t, 2, out.Plan.Budget.IndependentHosts)
	assert.Equal(t, 2, out.Plan.Budget.MaxIndependent)
}

// A lease that serves node and independent work is a node lease.
func TestMixedLeaseIsNode(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("h1", 10)
	b.wantsImageOf("h1", imgGeth, digestA, protocol.ImpactNode)
	b.wantsImageOf("h1", "org/exporter:latest", digestB, protocol.ImpactIndependent)

	b.step()
	require.Equal(t, []string{"h1"}, b.leasedHosts())
	assert.Len(t, b.lease("h1").Needs, 2)
	assert.Equal(t, protocol.ImpactNode, b.lease("h1").Impact)
	assert.Equal(t, uint64(10), b.lease("h1").Charged)
}

// A host marked known offline is left out of the gate and the budget, and
// gets no lease; when the mark ends it counts again.
func TestKnownOfflineHost(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("dead", 50)
	b.host("a", 50)
	b.host("b", 50)
	b.down("dead")
	b.wants("dead", packagesNeed("2.0"))
	b.wants("a", packagesNeed("2.0"))

	// A third of the validators do not attest: unmarked, that closes the gate.
	until := b.w.Now.Add(time.Hour)
	b.get("dead").Offline = &fleet.OfflineMark{Reason: "client kept broken for debugging", Until: &until}

	out := b.step()
	require.True(t, out.Plan.Gate.OK, "the marked host's validators are left out of the share")
	assert.Equal(t, uint64(100), out.Plan.Budget.TotalWeight)
	assert.Equal(t, 1, out.Plan.Budget.OfflineHosts)
	assert.Equal(t, uint64(50), out.Plan.Budget.OfflineWeight)
	assert.Equal(t, []string{"a"}, b.leasedHosts(), "the marked host gets no lease")
	assert.Equal(t, SlotOffline, out.Plan.Hosts["dead"].Slot)
	assert.Equal(t, "known offline", queueReasons(out.Plan)["dead"])

	// The end passes: the host counts again and closes the gate.
	b.advance(time.Hour)

	out = b.step()
	assert.Zero(t, out.Plan.Budget.OfflineHosts)
	assert.False(t, out.Plan.Gate.OK)
}

// What a host asks for itself goes before the fleet's rollouts, however
// old they are; the budget still applies.
func TestLocalRolloutsGoFirst(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxLeasedPercent = 50
	b.w.Settings.Paused = true

	for _, name := range []string{"a", "b"} {
		b.host(name, 10)
	}

	b.wants("a", packagesNeed("2.0"))
	b.step()

	b.advance(time.Minute)

	local := imageNeed("c", imgLighthouse, digestB)
	local.ID, local.Local = protocol.LocalNeedID("r1"), true
	b.wants("b", local)
	b.w.Settings.Paused = false

	out := b.step()
	assert.Equal(t, []string{"b"}, b.leasedHosts())
	assert.Equal(t, "validators down: 50% now, 100% with this host, limit 10%", queueReasons(out.Plan)["a"])
}

// A need that waits for its rollout's canary is not granted with a lease the
// host is granted for another rollout: a new image does not reach the hosts
// of a running reboot rollout before its canary has proved it.
func TestOrderWaitsDoNotRideAlong(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 40
	b.w.Settings.MaxLeasedPercent = 40

	hosts := []string{"a", "b", "c", "d", "e"}
	for _, name := range hosts {
		b.host(name, 10)
		b.wants(name, rebootNeed())
	}

	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts(), "the canary of the reboot rollout")
	b.finish("a")
	b.step()
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)

	// In the pass in which the reboot rollout has its first success, a geth
	// digest appears.
	for _, name := range hosts[1:] {
		b.wantsImage(name, imgGeth, digestA)
	}

	out := b.step()
	require.Nil(t, b.lease("a"))
	geth := b.mustRollout(imageKey("", imgGeth, digestA))

	require.Equal(t, "b", geth.Canary)
	assert.Equal(t, []string{"b", "c"}, b.leasedHosts())
	assert.Len(t, b.lease("b").Needs, 2, "the canary gets both")
	require.Len(t, b.lease("c").Needs, 1)
	assert.Equal(t, protocol.NeedReboot, b.lease("c").Needs[0].Kind)
	assert.Equal(t, fleet.HostPending, geth.Host("c").State)
	assert.Equal(t, "validators down: 40% now, 60% with this host, limit 40%", queueReasons(out.Plan)["d"])
}

// Validators without an attestation duty (exited, pending, beyond the
// validator set) count for nothing: a host whose range is half exited is
// fully available, keeps the gate open, and is ready when the validators
// that have a duty attest.
func TestValidatorsWithoutDutyCountForNothing(t *testing.T) {
	b := newWorldBuilder(t)
	b.host("half", 20)
	b.host("a", 10)
	b.host("b", 10)
	b.dutyOverride["half"] = 10
	b.liveOverride["half"] = 10
	b.wants("half", packagesNeed("2.0"))

	out := b.step()
	assert.True(t, out.Plan.Gate.OK, "participation is 30 of the 30 with a duty")
	assert.Equal(t, uint64(30), out.Plan.Budget.TotalWeight)
	assert.Equal(t, uint64(10), out.Plan.Hosts["half"].Weight, "the active validators")
	assert.Equal(t, uint64(10), out.Plan.Hosts["half"].Live)
	require.Equal(t, []string{"half"}, b.leasedHosts())
	assert.Equal(t, uint64(10), b.lease("half").Charged)

	b.serve("half")
	assert.Equal(t, fleet.LeaseReleased, b.lastLease("half").State, "9 of the 10 with a duty attest")

	out = b.step()
	assert.True(t, out.Plan.Gate.OK)
	assert.Zero(t, out.Plan.Budget.UnavailableWeight)

	t.Run("a host none of whose validators has a duty is a host without validators", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.host("exited", 10)
		b.host("a", 10)
		b.dutyOverride["exited"] = 0
		b.liveOverride["exited"] = 0
		b.wants("exited", packagesNeed("2.0"))

		out := b.step()
		assert.Zero(t, out.Plan.Hosts["exited"].Weight)
		assert.Equal(t, uint64(10), out.Plan.Budget.TotalWeight)
		require.Equal(t, []string{"exited"}, b.leasedHosts())
		assert.Zero(t, b.lease("exited").Charged)

		b.serve("exited")
		ended := b.lastLease("exited")
		assert.Equal(t, fleet.LeaseReleased, ended.State)
		assert.NotContains(t, ended.Readiness, noteAttesting, "no wait for attestations")
	})

	t.Run("a host none of whose validators has a duty does not pass the closed gate", func(t *testing.T) {
		b := newWorldBuilder(t)
		b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 5 }
		b.refreshHealth()
		b.host("exited", 10)
		b.host("a", 10)
		b.dutyOverride["exited"] = 0
		b.liveOverride["exited"] = 0
		b.wants("exited", packagesNeed("2.0"))

		out := b.step()
		assert.Empty(t, b.leasedHosts())
		assert.Equal(t, "network gate: finality lag 5 epochs > 3", queueReasons(out.Plan)["exited"])
	})
}

// An Ansible deploy rolls without approval whatever the image policies say,
// but not through a closed gate: there it waits for a person like any auto
// rollout.
func TestDeployIsAutoButWaitsAtAClosedGate(t *testing.T) {
	const closed = "network gate: finality lag 5 epochs > 3"

	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.MaxLeasedPercent = 100
	b.w.Settings.PolicyImage = fleet.PolicyManual
	b.w.Settings.PolicyClients = fleet.ClientPolicies{"el": {"geth": fleet.PolicyManual, "besu": fleet.PolicyManual}}
	b.host("a", 10)
	b.host("b", 10)
	b.deploysInGroup("a", "geth", protocol.KindEL, "geth", imgGeth, digestA)

	b.step()
	assert.NotNil(t, b.lease("a"), "no approval under the manual image policy")
	assert.False(t, b.openGroup(protocol.KindEL, "geth").Approved)

	b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 5 }
	b.refreshHealth()
	b.deploysInGroup("b", "besu", protocol.KindEL, "besu", "hyperledger/besu:develop", digestA)

	out := b.step()
	assert.Nil(t, b.lease("b"))
	assert.Equal(t, closed, queueReasons(out.Plan)["b"])

	b.serve("a")
	b.openGroup(protocol.KindEL, "besu").Approved = true
	b.step()
	assert.NotNil(t, b.lease("b"), "approved, it passes the closed gate")
}

// An image rollout waits for approval by the policy of its team, else of its
// kind (other for unlabelled containers), else policyImage.
func TestApprovalByKindPolicy(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxUnavailablePercent = 100
	b.w.Settings.MaxLeasedPercent = 100
	b.w.Settings.MaxIndependentPercent = 100
	b.w.Settings.PolicyClients = fleet.ClientPolicies{"el": {"geth": fleet.PolicyAuto}}
	b.w.Settings.PolicyKinds = fleet.KindPolicies{
		"el": fleet.PolicyManual, "snooper-el": fleet.PolicyManual, fleet.KindOther: fleet.PolicyManual,
	}

	for _, name := range []string{"a", "b", "c", "d", "e"} {
		b.host(name, 0)
	}

	b.wantsInGroup("a", "geth", protocol.KindEL, "geth", imgGeth, digestA)
	b.wantsInGroup("b", "besu", protocol.KindEL, "besu", "hyperledger/besu:develop", digestA)
	b.wantsInGroup("c", "snooper", "snooper-el", "", "ethpandaops/snooper:latest", digestA)
	b.wantsInGroup("d", "tools", "", "", imgVC, digestA)
	b.get("d").Status.Containers[0].Impact = protocol.ImpactIndependent
	b.wantsInGroup("e", "lighthouse", protocol.KindCL, "lighthouse", imgLighthouse, digestA)

	out := b.step()
	reasons := queueReasons(out.Plan)

	assert.NotNil(t, b.lease("a"), "the client's auto before the kind's manual")
	assert.NotNil(t, b.lease("e"), "a kind not listed follows policyImage")

	for _, host := range []string{"b", "c", "d"} {
		assert.Nil(t, b.lease(host), host)
		assert.Equal(t, reasonApproval, reasons[host], host)
	}
}

// While the initial deployment window is open nothing but Ansible deploys is
// granted; the window ends by time with no action.
func TestDeployWindowHoldsAllButDeploys(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxLeasedPercent = 100
	b.w.DeployWindow = fleet.DeployWindowState{StartedAt: b.w.Now.Add(-time.Hour), Length: 24 * time.Hour}
	end := b.w.DeployWindow.StartedAt.Add(24 * time.Hour)

	b.host("h", 10)
	b.wants("h", packagesNeed("2.0"))
	b.host("d", 10)
	b.deploysInGroup("d", "geth", protocol.KindEL, "geth", imgGeth, digestA)

	out := b.step()

	assert.Equal(t, []string{"d"}, b.leasedHosts(), "the deploy goes through the window")
	assert.Equal(t, DeployWindow{Open: true, Until: &end}, out.Plan.DeployWindow)
	require.Len(t, out.Plan.Queue, 1)
	assert.Equal(t, QueueEntry{
		Host: "h", Position: 1, Reason: "initial deployment window until " + end.UTC().Format(time.RFC3339),
		Wait: WaitDeployWindow, Rollouts: out.Plan.Queue[0].Rollouts,
	}, out.Plan.Queue[0])

	b.serve("d")
	b.advance(end.Sub(b.w.Now) - time.Second)
	b.step()
	assert.Nil(t, b.lease("h"), "a second before the end")

	b.advance(time.Second)
	out = b.step()
	assert.NotNil(t, b.lease("h"), "granted on the first pass after the end")
	assert.False(t, out.Plan.DeployWindow.Open)
}

// The global pause holds deploys too, and says so before the window.
func TestDeployWindowUnderPause(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.Paused = true
	b.w.DeployWindow = fleet.DeployWindowState{StartedAt: b.w.Now, Length: 24 * time.Hour}

	b.host("h", 10)
	b.wants("h", packagesNeed("2.0"))
	b.host("d", 10)
	b.deploysInGroup("d", "geth", protocol.KindEL, "geth", imgGeth, digestA)

	out := b.step()

	assert.Empty(t, b.leasedHosts())
	assert.Equal(t, map[string]string{"h": "paused", "d": "paused"}, queueReasons(out.Plan))
}

// An armed window, a disabled one and a store without one grant as usual.
func TestDeployWindowClosed(t *testing.T) {
	for name, mod := range map[string]func(b *worldBuilder){
		"armed": func(b *worldBuilder) {
			at := b.w.Now
			b.w.DeployWindow = fleet.DeployWindowState{StartedAt: b.w.Now, Length: 24 * time.Hour, ArmedAt: &at}
		},
		"disabled": func(b *worldBuilder) {
			b.w.DeployWindow = fleet.DeployWindowState{StartedAt: b.w.Now}
		},
		"no window stored": func(*worldBuilder) {},
	} {
		t.Run(name, func(t *testing.T) {
			b := newWorldBuilder(t)
			mod(b)
			b.host("h", 10)
			b.wants("h", packagesNeed("2.0"))

			out := b.step()

			assert.NotNil(t, b.lease("h"))
			assert.False(t, out.Plan.DeployWindow.Open)
		})
	}
}
