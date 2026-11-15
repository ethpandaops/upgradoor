package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// queryPlan returns the lines of EXPLAIN QUERY PLAN for a query.
func queryPlan(t *testing.T, s Store, query string, args []any) []string {
	t.Helper()

	rows, err := rawDB(t, s).Query(`EXPLAIN QUERY PLAN `+query, args...)
	require.NoError(t, err)

	defer func() { require.NoError(t, rows.Close()) }()

	var plan []string

	for rows.Next() {
		var (
			id, parent, unused int
			detail             string
		)

		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))

		plan = append(plan, detail)
	}

	require.NoError(t, rows.Err())

	return plan
}

// fillHistory stores ended leases, closed rollouts and answered directives,
// and a few open ones, so that the planner has something to choose between.
func fillHistory(t *testing.T, s Store, ended int) {
	t.Helper()

	changes := &Changes{}

	for i := range ended {
		host := fmt.Sprintf("node-%d", i%8)
		state := []fleet.LeaseState{fleet.LeaseReleased, fleet.LeaseFailed, fleet.LeaseExpired}[i%3]
		closed := []fleet.RolloutState{
			fleet.RolloutCompleted, fleet.RolloutSuperseded, fleet.RolloutAborted,
		}[i%3]
		answered := []fleet.DirectiveState{
			fleet.DirectiveAccepted, fleet.DirectiveRejected, fleet.DirectiveCancelled,
		}[i%3]

		changes.Leases = append(changes.Leases, testLease(fmt.Sprintf("ended-%d", i), host, state, i))
		changes.Rollouts = append(changes.Rollouts, testRollout(fmt.Sprintf("closed-%d", i), closed, i, host))
		changes.Directives = append(changes.Directives,
			testDirective(fmt.Sprintf("answered-%d", i), host, answered, i))
	}

	for i, state := range fleet.OpenLeaseStates() {
		changes.Leases = append(changes.Leases,
			testLease(fmt.Sprintf("open-%d", i), fmt.Sprintf("node-%d", i), state, ended+i))
	}

	changes.Rollouts = append(changes.Rollouts,
		testRollout("active", fleet.RolloutActive, ended, "node-0"),
		testRollout("halted", fleet.RolloutHalted, ended+1, "node-1"))
	changes.Directives = append(changes.Directives,
		testDirective("pending", "node-0", fleet.DirectivePending, ended))

	require.NoError(t, s.Commit(context.Background(), changes))

	_, err := rawDB(t, s).Exec(`ANALYZE`)
	require.NoError(t, err)
}

// M4: what the scheduler reads on every tick, and what the API reads on every
// sync of an agent, must not read a whole table that only ever grows. The
// lease table keeps every lease that ended, and "state NOT IN (...)" cannot
// use the index on state: every tick scanned it.
func TestTickQueriesDoNotScanTheHistory(t *testing.T) {
	tick := []struct {
		name  string
		table string
		// search is what the plan must say about the table: which index it
		// searches. The absence of a scan alone would also hold for a plan
		// that does not mention the table at all.
		search string
		query  func() (string, []any)
	}{
		{"open leases", "leases", "SEARCH leases USING INDEX leases_state (state=?)", func() (string, []any) {
			return leasesQuery(LeaseFilter{OpenOnly: true})
		}},
		// By the host or by the state, whichever the statistics favour.
		{"the open lease of a host", "leases", "SEARCH leases USING INDEX leases_", func() (string, []any) {
			return leasesQuery(LeaseFilter{OpenOnly: true, Host: "node-1"})
		}},
		{"open rollouts", "rollouts", "SEARCH rollouts USING INDEX rollouts_state (state=?)", func() (string, []any) {
			return rolloutsQuery(RolloutFilter{OpenOnly: true})
		}},
		{"members of a rollout", "rollout_hosts", "SEARCH rollout_hosts USING PRIMARY KEY (rollout_id=?)",
			func() (string, []any) {
				return rolloutHostsQuery, []any{"active"}
			}},
		{"pending directives", "directives", "SEARCH directives USING INDEX directives_state (state=?)",
			func() (string, []any) {
				return directivesQuery(DirectiveFilter{PendingOnly: true})
			}},
		{"pending directives of a host", "directives",
			"SEARCH directives USING INDEX directives_host (host=? AND state=?)", func() (string, []any) {
				return directivesQuery(DirectiveFilter{PendingOnly: true, Host: "node-1"})
			}},
		{"a lease by its ID", "leases", "SEARCH leases USING INDEX sqlite_autoindex_leases_1 (id=?)",
			func() (string, []any) {
				return `SELECT data FROM leases WHERE id = ?`, []any{"open-0"}
			}},
		{"newest liveness", "validator_liveness",
			"SEARCH validator_liveness USING INDEX validator_liveness_epoch (epoch=?)", func() (string, []any) {
				return livenessQuery, []any{6}
			}},
	}

	// With and without statistics: a database that was never analysed must
	// choose the index too.
	for _, filled := range []bool{false, true} {
		for _, backend := range backends {
			t.Run(fmt.Sprintf("%s/filled=%t", backend.name, filled), func(t *testing.T) {
				s := backend.open(t)

				if filled {
					fillHistory(t, s, 600)
				}

				for _, q := range tick {
					query, args := q.query()
					plan := queryPlan(t, s, query, args)
					require.NotEmpty(t, plan, q.name)
					assert.Contains(t, strings.Join(plan, "\n"), q.search,
						"%s does not search the table by the index it is meant to use", q.name)

					for _, line := range plan {
						// "SCAN <table>" reads the table; "SCAN <table> USING
						// COVERING INDEX" walks an index, which the liveness query
						// does for the newest epochs only (LIMIT).
						scans := strings.HasPrefix(line, "SCAN "+q.table) &&
							!strings.Contains(line, "USING COVERING INDEX")
						assert.False(t, scans, "%s reads the whole table: %s\n%s",
							q.name, line, strings.Join(plan, "\n"))
					}
				}
			})
		}
	}
}

// The open states of the queries are the ones Terminal does not name.
func TestOpenQueriesReturnExactlyTheOpenRows(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			fillHistory(t, s, 30)

			leases, err := s.ListLeases(ctx, LeaseFilter{OpenOnly: true})
			require.NoError(t, err)
			require.Len(t, leases, 3)

			for _, l := range leases {
				assert.False(t, l.State.Terminal(), l.ID)
			}

			all, err := s.ListLeases(ctx, LeaseFilter{})
			require.NoError(t, err)

			open := 0

			for _, l := range all {
				if !l.State.Terminal() {
					open++
				}
			}

			assert.Equal(t, 3, open)
			assert.Len(t, all, 33)

			rollouts, err := s.ListRollouts(ctx, RolloutFilter{OpenOnly: true})
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"active", "halted"}, rolloutIDs(rollouts))
		})
	}

	for _, state := range fleet.LeaseStates() {
		assert.Equal(t, !state.Terminal(), containsState(fleet.OpenLeaseStates(), state), state)
	}
}

func containsState(states []fleet.LeaseState, state fleet.LeaseState) bool {
	for _, s := range states {
		if s == state {
			return true
		}
	}

	return false
}

// A commit sets holds in the transaction of its leases: either both are
// stored or neither.
func TestCommitHolds(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			_, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-1", nil), Request: testRequest(), At: at(0)})
			require.NoError(t, err)

			lease := testLease("l1", "node-1", fleet.LeaseRunning, 10)
			lease.Cancelled = &fleet.LeaseCancel{
				At: at(20).Add(1500 * 1000), By: "alice", State: fleet.LeaseRunning, Reason: "taken away",
			}

			require.NoError(t, s.Commit(ctx, &Changes{
				Leases: []*fleet.Lease{lease},
				Holds:  []HostHold{{Host: "node-1", Wanted: true, Reason: "lease ended by operator"}},
			}))

			host, err := s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			assert.True(t, host.HoldWanted)
			assert.Equal(t, "lease ended by operator", host.HoldReason)

			got, err := s.GetLease(ctx, "l1")
			require.NoError(t, err)
			require.NotNil(t, got.Cancelled)
			assert.Equal(t, "alice", got.Cancelled.By)
			assert.Equal(t, fleet.LeaseRunning, got.Cancelled.State)
			assert.True(t, got.Cancelled.At.Equal(storedTime(lease.Cancelled.At)), "millisecond precision")
			assert.True(t, lease.Cancelled.At.Equal(at(20).Add(1500*1000)), "the caller's lease is not changed")

			// A hold of a host that does not exist fails the whole commit.
			other := testLease("l2", "node-2", fleet.LeaseRunning, 30)
			err = s.Commit(ctx, &Changes{
				Leases: []*fleet.Lease{other},
				Holds:  []HostHold{{Host: "node-2", Wanted: true, Reason: "x"}},
			})
			require.ErrorIs(t, err, ErrNotFound)

			_, err = s.GetLease(ctx, "l2")
			require.ErrorIs(t, err, ErrNotFound)

			assert.False(t, (&Changes{Holds: []HostHold{{Host: "node-1"}}}).Empty())
		})
	}
}
