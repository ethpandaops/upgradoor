package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

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

	b.get("t2").Status.Hold.Holds = []protocol.HostHold{{Holder: fleet.HolderStaff}}
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

// A host whose agent reports an operation that is not over, while the host
// holds no lease, takes a slot: the budget and the host limit count it and it
// is granted nothing. No lease is made for the operation. A lease that failed
// as silent and whose agent is back, and a lease of a lost database, both look
// like this.
func TestHostWorkingWithoutALeaseTakesASlot(t *testing.T) {
	b := newWorldBuilder(t)

	// Ten hosts of ten validators: the budget of 10% is one host.
	for i := range 10 {
		name := fmt.Sprintf("h%d", i)
		b.host(name, 10)
		b.wants(name, packagesNeed("2.0"))
	}

	st := *b.get("h3").Status
	st.Operation = &protocol.Operation{LeaseID: "unknown", Phase: protocol.PhaseApplying, Disrupted: true}
	b.get("h3").Status = &st

	out := b.step()
	assert.Empty(t, out.Leases, "h3 has the one slot, and gets no lease for its operation")
	assert.Equal(t, 1, out.Plan.Budget.LeasedHosts)
	assert.Equal(t, uint64(10), out.Plan.Budget.UnavailableWeight)
	assert.Equal(t, SlotLeased, out.Plan.Hosts["h3"].Slot)
	assert.Empty(t, out.Plan.Hosts["h3"].LeaseID)

	waits := make(map[string]WaitKind, len(out.Plan.Queue))
	for _, q := range out.Plan.Queue {
		waits[q.Host] = q.Wait
	}

	assert.Equal(t, reasonWorking, queueReasons(out.Plan)["h3"])
	assert.Equal(t, WaitWorking, waits["h3"])
	assert.Equal(t, WaitBudget, waits["h0"], "the canary waits for the slot h3 has")

	// A host that is silent says nothing: what it said last is not counted.
	b.advance(3*time.Minute, "h3")

	out = b.step()
	assert.Equal(t, []string{"h0"}, b.leasedHosts())
	assert.Equal(t, reasonAgentSilent, queueReasons(out.Plan)["h3"])
	assert.Equal(t, 1, out.Plan.Budget.LeasedHosts)
}

// largeWorld is a fleet of n hosts of 32 validators that all report the same
// packages need, reconciled until the host limit of leases is open: one
// rollout with every host as a member, a tenth of them leased and at work.
func largeWorld(tb testing.TB, n int) *World {
	tb.Helper()

	ids := 0
	now := testGenesis.Add(100*testEpoch + 10*time.Second)
	epoch := uint64(now.Sub(testGenesis) / testEpoch)
	w := &World{
		Now:      now,
		Settings: fleet.DefaultSettings(),
		Health: fleet.NetworkHealth{
			UpdatedAt: now, GenesisTime: testGenesis, SlotsPerEpoch: testSlotsPerEpoch,
			Phases:       fleet.SinglePhase(testGenesis, testSecondsPerSlot*time.Second),
			CurrentEpoch: epoch, FinalizedEpoch: epoch - 2, FinalityLagEpochs: 2,
			LivenessKnown: true, LivenessEpoch: epoch - 1,
		},
		NewID: func() string {
			ids++

			return fmt.Sprintf("id%06d", ids)
		},
	}

	for i := range n {
		name := fmt.Sprintf("host-%05d", i)
		start := uint64(i) * 32
		w.Hosts = append(w.Hosts, &fleet.Host{
			Name: name, LastSeen: now,
			Validators: []identity.ValidatorRange{{Start: start, End: start + 32}},
			Status: &protocol.SyncRequest{
				Verbs: allVerbs, Needs: []protocol.Need{packagesNeed("2.0")},
				Node: protocol.NodeHealth{
					EL: &protocol.ELHealth{Reachable: true, Peers: 20},
					CL: &protocol.CLHealth{Reachable: true, Peers: 20},
				},
			},
		})
		w.Liveness = append(w.Liveness, fleet.Liveness{Host: name, Epoch: epoch - 1, Live: 32, Total: 32})
	}

	// The canary, its release, then the rest up to the host limit.
	applyOutcome(w, Reconcile(w))

	for _, r := range w.Rollouts {
		for i := range r.Hosts {
			if r.Hosts[i].State == fleet.HostLeased {
				r.Hosts[i].State = fleet.HostDone
			}
		}
	}

	w.Leases = nil
	applyOutcome(w, Reconcile(w))

	if want := n / 10; len(w.Leases) != want {
		tb.Fatalf("%d leases are open, the host limit is %d", len(w.Leases), want)
	}

	for _, l := range w.Leases {
		for _, h := range w.Hosts {
			if h.Name == l.Host {
				st := *h.Status
				st.Operation = &protocol.Operation{LeaseID: l.ID, Phase: protocol.PhaseApplying, Disrupted: true}
				h.Status = &st
			}
		}
	}

	return w
}

// BenchmarkReconcile is one reconcile of a large fleet in the middle of a
// rollout: it must stay about linear in the number of hosts.
func BenchmarkReconcile(b *testing.B) {
	for _, n := range []int{1000, 5000} {
		w := largeWorld(b, n)

		b.Run(fmt.Sprintf("hosts=%d", n), func(b *testing.B) {
			for b.Loop() {
				Reconcile(w)
			}
		})
	}
}
