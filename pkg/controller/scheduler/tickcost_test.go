package scheduler

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// historyStore is a SQLite store with hosts that have synced and, per host,
// a number of leases that ended long ago.
func historyStore(tb testing.TB, hosts, ended int, now time.Time) store.Store {
	tb.Helper()

	ctx := context.Background()
	st := store.NewMemory(quietLog())
	require.NoError(tb, st.Start(ctx))
	tb.Cleanup(func() { require.NoError(tb, st.Stop()) })

	need := packagesNeed("2.0")

	for h := range hosts {
		name := fmt.Sprintf("host-%03d", h)

		_, err := st.RecordSync(ctx, &store.HostSync{
			Cert: &identity.Certificate{
				Claims: identity.Claims{Issuer: identity.Issuer, Network: "testnet-0", Subject: name, Generation: 1},
				Hash:   "hash-" + name,
			},
			Request: &protocol.SyncRequest{ProtocolVersion: protocol.Version, Verbs: slices.Clone(allVerbs)},
			At:      now,
		})
		require.NoError(tb, err)

		changes := &store.Changes{Leases: make([]*fleet.Lease, 0, ended)}

		for i := range ended {
			at := now.Add(-time.Duration(ended-i) * time.Hour)
			state := []fleet.LeaseState{fleet.LeaseReleased, fleet.LeaseExpired, fleet.LeaseFailed}[i%3]

			changes.Leases = append(changes.Leases, &fleet.Lease{
				ID: fmt.Sprintf("%s-%05d", name, i), Host: name, State: state,
				Needs: []protocol.Need{need}, Rollouts: map[string]string{need.ID: "long-gone"},
				GrantedBy: actorScheduler, GrantedAt: at.Add(-time.Minute), ExpiresAt: at, EndedAt: &at,
				Operation: &protocol.Operation{
					LeaseID: name, Phase: protocol.PhaseDone, Disrupted: true,
					Steps: []protocol.Step{{NeedID: need.ID, Status: protocol.StepDone}},
				},
			})
		}

		if ended > 0 {
			require.NoError(tb, st.Commit(ctx, changes))
		}
	}

	return st
}

// tickCost is the time of one reconcile tick over the store, once the
// scheduler has read its history: the fastest of several, so that a pause of
// the machine does not count.
func tickCost(tb testing.TB, st store.Store, now time.Time) time.Duration {
	tb.Helper()

	svc, ok := New(quietLog(), Options{
		Store:    st,
		Health:   func() fleet.NetworkHealth { return fleet.NetworkHealth{} },
		Defaults: fleet.DefaultSettings(),
		Now:      func() time.Time { return now },
	}).(*service)
	require.True(tb, ok)

	ctx := context.Background()

	// The first tick after a start reads the lease table once.
	require.NoError(tb, svc.reconcile(ctx))

	best := time.Duration(0)

	for range 15 {
		start := time.Now()

		require.NoError(tb, svc.reconcile(ctx))

		if took := time.Since(start); best == 0 || took < best {
			best = took
		}
	}

	return best
}

// M4: a tick costs what is open, not what has ended. The lease table is never
// pruned; with "state NOT IN (...)" every tick read all of it. Ten times the
// history must not make a tick noticeably slower.
func TestTickCostDoesNotGrowWithTheLeaseHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a database with 110000 leases")
	}

	// It compares times, and under the race detector it takes a minute to
	// say nothing: "make test" runs it once more without the detector.
	if raceDetector {
		t.Skip("measures time: run without -race (make test-go does)")
	}

	const hosts = 200

	now := testGenesis.Add(100 * testEpoch)
	small := tickCost(t, historyStore(t, hosts, 50, now), now)
	large := tickCost(t, historyStore(t, hosts, 500, now), now)

	t.Logf("tick with %d ended leases: %s; with %d: %s", hosts*50, small, hosts*500, large)

	// A full scan of the table costs many times a tick; the allowance is for
	// the noise of a loaded machine.
	if large > 3*small+2*time.Millisecond {
		t.Errorf("a tick over %d ended leases takes %s, over %d it takes %s: the cost grows with the history",
			hosts*500, large, hosts*50, small)
	}
}

// BenchmarkTick measures one reconcile tick of 200 hosts over lease histories
// of different lengths.
func BenchmarkTick(b *testing.B) {
	now := testGenesis.Add(100 * testEpoch)

	for _, ended := range []int{0, 50, 500} {
		b.Run(fmt.Sprintf("ended=%d", 200*ended), func(b *testing.B) {
			st := historyStore(b, 200, ended, now)

			svc, ok := New(quietLog(), Options{
				Store:    st,
				Health:   func() fleet.NetworkHealth { return fleet.NetworkHealth{} },
				Defaults: fleet.DefaultSettings(),
				Now:      func() time.Time { return now },
			}).(*service)
			require.True(b, ok)
			require.NoError(b, svc.reconcile(context.Background()))

			b.ResetTimer()

			for range b.N {
				if err := svc.reconcile(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
