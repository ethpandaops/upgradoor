package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

func auditIDs(entries []fleet.AuditEntry) []int64 {
	ids := make([]int64, 0, len(entries))
	for i := range entries {
		ids = append(ids, entries[i].ID)
	}

	return ids
}

func TestAppendAudit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			s := backend.open(t)

			empty, err := s.ListAudit(ctx, AuditFilter{})
			require.NoError(t, err)
			assert.NotNil(t, empty)
			assert.Empty(t, empty)

			require.NoError(t, s.AppendAudit(ctx), "no entries is a no-op")

			detail := map[string]any{
				"before": map[string]any{"paused": false, "readyAttestations": float64(1)},
				"after":  map[string]any{"paused": true, "readyAttestations": float64(2)},
				"note":   "quotes \" and ' and ü",
				"list":   []any{"a", float64(1), nil},
			}

			before := time.Now().Add(-time.Second)

			require.NoError(t, s.AppendAudit(ctx,
				fleet.AuditEntry{
					ID: 999, Time: at(5), Actor: "alice@example.org", Action: "settings.update",
					Target: "paused", Detail: detail,
				},
				fleet.AuditEntry{Actor: "scheduler", Action: "lease.granted", Target: "node-1"},
			))
			require.NoError(t, s.AppendAudit(ctx, fleet.AuditEntry{Actor: "anonymous", Action: "action.roll"}))

			got, err := s.ListAudit(ctx, AuditFilter{})
			require.NoError(t, err)
			require.Len(t, got, 3)

			assert.Equal(t, []int64{3, 2, 1}, auditIDs(got), "newest first; a caller's ID is ignored")
			assert.Equal(t, fleet.AuditEntry{
				ID: 1, Time: at(5), Actor: "alice@example.org", Action: "settings.update",
				Target: "paused", Detail: detail,
			}, got[2])

			assert.Equal(t, "lease.granted", got[1].Action)
			assert.Nil(t, got[1].Detail, "no detail stays nil")
			assert.WithinRange(t, got[1].Time, before, time.Now().Add(time.Second), "zero time is set to now")
			assert.Empty(t, got[0].Target)

			// One bad entry: none of the call is stored.
			err = s.AppendAudit(ctx,
				fleet.AuditEntry{Actor: "scheduler", Action: "ok"},
				fleet.AuditEntry{Actor: "scheduler", Action: "bad", Detail: map[string]any{"x": math.Inf(1)}},
			)
			require.Error(t, err)
			assert.Equal(t, 3, countRows(t, s, "audit"))

			// IDs keep growing and are never reused, also after a rollback.
			require.NoError(t, s.AppendAudit(ctx, fleet.AuditEntry{Actor: "scheduler", Action: "next"}))

			got, err = s.ListAudit(ctx, AuditFilter{Limit: 1})
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Greater(t, got[0].ID, int64(3))
		})
	}
}

func TestListAudit(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	// IDs 1..12, cycling through four actions and three targets.
	actions := []string{"lease.granted", "lease.released", "action.roll", "Lease.Odd"}
	targets := []string{"node-1", "node-2", ""}

	entries := make([]fleet.AuditEntry, 0, 12)
	for i := range 12 {
		entries = append(entries, fleet.AuditEntry{
			Time: at(i), Actor: "scheduler", Action: actions[i%4], Target: targets[i%3],
		})
	}

	require.NoError(t, s.AppendAudit(ctx, entries...))
	require.NoError(t, s.AppendAudit(ctx,
		fleet.AuditEntry{Time: at(100), Actor: "scheduler", Action: "100%_done", Target: "node-%"}))

	tests := []struct {
		name   string
		filter AuditFilter
		want   []int64
	}{
		{"everything, newest first", AuditFilter{}, []int64{13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}},
		{"limit", AuditFilter{Limit: 3}, []int64{13, 12, 11}},
		{"target", AuditFilter{Target: "node-1"}, []int64{10, 7, 4, 1}},
		{"target is exact, not a pattern", AuditFilter{Target: "node-%"}, []int64{13}},
		{"unknown target", AuditFilter{Target: "nope"}, []int64{}},
		{"action prefix", AuditFilter{ActionPrefix: "lease."}, []int64{10, 9, 6, 5, 2, 1}},
		{"action prefix is case-sensitive", AuditFilter{ActionPrefix: "Lease."}, []int64{12, 8, 4}},
		{"full action", AuditFilter{ActionPrefix: "action.roll"}, []int64{11, 7, 3}},
		{"prefix longer than any action", AuditFilter{ActionPrefix: "action.rollback"}, []int64{}},
		{"prefix wildcards are literal", AuditFilter{ActionPrefix: "100%"}, []int64{13}},
		{"prefix underscore is literal", AuditFilter{ActionPrefix: "lease_"}, []int64{}},
		{"target and prefix", AuditFilter{Target: "node-2", ActionPrefix: "lease."}, []int64{5, 2}},
		{"before id", AuditFilter{BeforeID: 4}, []int64{3, 2, 1}},
		{"before the first", AuditFilter{BeforeID: 1}, []int64{}},
		{"negative before id is ignored", AuditFilter{BeforeID: -3, Limit: 2}, []int64{13, 12}},
		{"all filters", AuditFilter{Target: "node-1", ActionPrefix: "lease.", BeforeID: 10, Limit: 1}, []int64{1}},
		{"negative limit means the default", AuditFilter{Limit: -1}, []int64{13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListAudit(ctx, tt.filter)
			require.NoError(t, err)
			assert.Equal(t, tt.want, auditIDs(got))
		})
	}

	t.Run("paging with before id visits everything once", func(t *testing.T) {
		seen := make([]int64, 0, 13)
		filter := AuditFilter{Limit: 5}

		for {
			page, err := s.ListAudit(ctx, filter)
			require.NoError(t, err)

			if len(page) == 0 {
				break
			}

			seen = append(seen, auditIDs(page)...)
			filter.BeforeID = page[len(page)-1].ID
		}

		assert.Equal(t, []int64{13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, seen)
	})
}

func TestListAuditLimits(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	entries := make([]fleet.AuditEntry, 0, MaxAuditLimit+50)
	for i := range MaxAuditLimit + 50 {
		entries = append(entries, fleet.AuditEntry{
			Time: at(i), Actor: "scheduler", Action: "sync", Target: fmt.Sprintf("node-%d", i%7),
		})
	}

	require.NoError(t, s.AppendAudit(ctx, entries...))

	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{"zero means the default", 0, DefaultAuditLimit},
		{"below the default", 10, 10},
		{"above the default", 250, 250},
		{"the maximum", MaxAuditLimit, MaxAuditLimit},
		{"capped at the maximum", MaxAuditLimit + 1, MaxAuditLimit},
		{"far above", 1 << 30, MaxAuditLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListAudit(ctx, AuditFilter{Limit: tt.limit})
			require.NoError(t, err)
			require.Len(t, got, tt.want)
			assert.Equal(t, int64(MaxAuditLimit+50), got[0].ID)
			assert.IsDecreasing(t, auditIDs(got))
		})
	}
}

// auditFixture stores a log as a fleet writes it: the scheduler's entries,
// an agent's registration and a few of people, one minute apart.
func auditFixture(t *testing.T, s Store) {
	t.Helper()

	entries := []fleet.AuditEntry{
		{Actor: "agent:node-1", Action: "host.registered", Target: "node-1"},                              // 1
		{Actor: "scheduler", Action: "rollout.created", Target: "r-1"},                                    // 2
		{Actor: "scheduler", Action: "lease.granted", Target: "node-1", Detail: map[string]any{"x": "y"}}, // 3
		{Actor: "alice@example.org", Action: "action.hold", Target: "node-2"},                             // 4
		{Actor: "scheduler", Action: "lease.released", Target: "node-1"},                                  // 5
		{Actor: "bob@example.org", Action: "settings.update", Target: "settings"},                         // 6
		{Actor: "alice@example.org", Action: "action.pause", Target: "settings"},                          // 7
		{Actor: "alice@example.org", Action: "action.pause", Target: "r-1"},                               // 8
		{Actor: "alice@example.org", Action: "action.approve-lease", Target: "node-1",
			Detail: map[string]any{"reason": "Looks Fine"}}, // 9
		{Actor: "scheduler", Action: "directive.accepted", Target: "node-2"},     // 10
		{Actor: "agent:node-3", Action: "host.registered", Target: "node-3"},     // 11
		{Actor: "anonymous", Action: "auth.refused", Target: "/api/v1/settings"}, // 12
	}

	for i := range entries {
		entries[i].Time = at(60 * i)
	}

	require.NoError(t, s.AppendAudit(context.Background(), entries...))
}

func TestListAuditFilters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newFileStore(t)
	auditFixture(t, s)

	tests := []struct {
		name   string
		filter AuditFilter
		want   []int64
	}{
		{"people", AuditFilter{ActorKind: fleet.AuditPeople}, []int64{12, 9, 8, 7, 6, 4}},
		{"scheduler", AuditFilter{ActorKind: fleet.AuditScheduler}, []int64{10, 5, 3, 2}},
		{"agents", AuditFilter{ActorKind: fleet.AuditAgents}, []int64{11, 1}},
		{"one person", AuditFilter{Actor: "alice@example.org"}, []int64{9, 8, 7, 4}},
		{"one agent", AuditFilter{Actor: "agent:node-3"}, []int64{11}},
		{"person and kind disagree", AuditFilter{Actor: "alice@example.org", ActorKind: fleet.AuditScheduler}, []int64{}},
		{"group leases", AuditFilter{Group: fleet.AuditGroupLeases}, []int64{9, 5, 3}},
		{"group leases of people", AuditFilter{Group: fleet.AuditGroupLeases, ActorKind: fleet.AuditPeople}, []int64{9}},
		{"group rollouts leaves out the global pause", AuditFilter{Group: fleet.AuditGroupRollouts}, []int64{8, 2}},
		{"group settings takes the global pause", AuditFilter{Group: fleet.AuditGroupSettings}, []int64{7, 6}},
		{"group holds", AuditFilter{Group: fleet.AuditGroupHolds}, []int64{4}},
		{"kind host", AuditFilter{TargetKind: fleet.AuditTargetHost}, []int64{11, 10, 9, 5, 4, 3, 1}},
		{"kind settings", AuditFilter{TargetKind: fleet.AuditTargetSetting}, []int64{7, 6}},
		{"kind request", AuditFilter{TargetKind: fleet.AuditTargetRequest}, []int64{12}},
		{"target and group", AuditFilter{Target: "node-1", Group: fleet.AuditGroupLeases}, []int64{9, 5, 3}},
		{"target settings and group rollouts", AuditFilter{Target: "settings", Group: fleet.AuditGroupRollouts}, []int64{}},
		{"from", AuditFilter{From: at(60 * 9)}, []int64{12, 11, 10}},
		{"to", AuditFilter{To: at(60 * 3)}, []int64{3, 2, 1}},
		{"from and to", AuditFilter{From: at(60*4 + 1), To: at(60 * 7)}, []int64{7, 6}},
		{"range of people", AuditFilter{From: at(60 * 4), To: at(60 * 9), ActorKind: fleet.AuditPeople}, []int64{9, 8, 7, 6}},
		{"search target", AuditFilter{Search: "NODE-2"}, []int64{10, 4}},
		{"search actor", AuditFilter{Search: "bob@"}, []int64{6}},
		{"search leaves out the detail", AuditFilter{Search: "looks fine"}, []int64{}},
		{"search the detail", AuditFilter{Search: "looks fine", SearchDetail: true}, []int64{9}},
		{"before and people", AuditFilter{ActorKind: fleet.AuditPeople, BeforeID: 8, Limit: 2}, []int64{7, 6}},
		{"sparse prefix", AuditFilter{ActionPrefix: "action.", ActorKind: fleet.AuditPeople}, []int64{9, 8, 7, 4}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := s.ListAudit(ctx, tt.filter)
			require.NoError(t, err)
			assert.Equal(t, tt.want, auditIDs(got))
		})
	}

	t.Run("actors", func(t *testing.T) {
		t.Parallel()

		people, agents, err := s.AuditActors(ctx)
		require.NoError(t, err)
		assert.Equal(t, []AuditActor{
			{Actor: "alice@example.org", Entries: 4}, {Actor: "anonymous", Entries: 1},
			{Actor: "bob@example.org", Entries: 1},
		}, people)
		assert.Equal(t, 2, agents)
	})
}

// A filter is served from the index that selects fewest entries: the log is
// mostly the scheduler's, and pages are read newest first.
func TestAuditQueriesUseTheirIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		filter AuditFilter
		// index is the one the plan names; "" is the table backwards by ID.
		index string
		// sorted: the plan sorts what the index selects.
		sorted bool
	}{
		{"everything", AuditFilter{}, "", false},
		{"people", AuditFilter{ActorKind: fleet.AuditPeople}, "audit_people", true},
		{"one person", AuditFilter{Actor: "alice@example.org"}, "audit_people", false},
		{"people's leases", AuditFilter{ActorKind: fleet.AuditPeople, Group: fleet.AuditGroupLeases}, "audit_people", true},
		{"agents", AuditFilter{ActorKind: fleet.AuditAgents}, "audit_agents", true},
		{"target", AuditFilter{Target: "node-1", ActorKind: fleet.AuditPeople}, "audit_target", false},
		{"settings", AuditFilter{Group: fleet.AuditGroupSettings}, "audit_target", false},
		{"holds", AuditFilter{Group: fleet.AuditGroupHolds}, "audit_action", true},
		{"leases", AuditFilter{Group: fleet.AuditGroupLeases}, "", false},
		{"scheduler", AuditFilter{ActorKind: fleet.AuditScheduler}, "", false},
		{"hosts", AuditFilter{TargetKind: fleet.AuditTargetHost}, "", false},
		{"action prefix", AuditFilter{ActionPrefix: "action.ho"}, "audit_action", true},
		{"time range", AuditFilter{From: at(10), To: at(20)}, "", false},
		{"search", AuditFilter{Search: "x"}, "", false},
	}

	for _, filled := range []bool{false, true} {
		for _, backend := range backends {
			t.Run(fmt.Sprintf("%s/filled=%t", backend.name, filled), func(t *testing.T) {
				t.Parallel()

				s := backend.open(t)

				if filled {
					auditFixture(t, s)
					_, err := rawDB(t, s).Exec(`ANALYZE`)
					require.NoError(t, err)
				}

				for _, tt := range tests {
					query, args := auditQuery(&tt.filter, 50, 3, 9)
					plan := strings.Join(queryPlan(t, s, query, args), "\n")

					if tt.index == "" {
						assert.NotContains(t, plan, "INDEX", "%s:\n%s", tt.name, plan)
					} else {
						assert.Regexp(t, `INDEX `+tt.index+`\b`, plan, "%s:\n%s", tt.name, plan)
					}

					assert.Equal(t, tt.sorted, strings.Contains(plan, "TEMP B-TREE"), "%s:\n%s", tt.name, plan)
				}
			})
		}
	}
}

// DeleteAuditBefore takes the oldest entries first, a batch at a time; the
// rest keep their IDs, pages and target index, and new IDs are not reused.
func TestDeleteAuditBefore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			s := backend.open(t)

			for i := range 6 {
				require.NoError(t, s.AppendAudit(ctx, fleet.AuditEntry{
					Time: at(i * 10), Actor: "scheduler", Action: "lease.granted", Target: "node-1",
				}))
			}

			n, err := s.DeleteAuditBefore(ctx, at(35), 0)
			require.NoError(t, err)
			assert.Zero(t, n, "limit 0 deletes nothing")

			n, err = s.DeleteAuditBefore(ctx, at(35), 3)
			require.NoError(t, err)
			assert.Equal(t, int64(3), n)

			n, err = s.DeleteAuditBefore(ctx, at(35), 3)
			require.NoError(t, err)
			assert.Equal(t, int64(1), n)

			n, err = s.DeleteAuditBefore(ctx, at(35), 3)
			require.NoError(t, err)
			assert.Zero(t, n)

			left, err := s.ListAudit(ctx, AuditFilter{Target: "node-1"})
			require.NoError(t, err)
			assert.Equal(t, []int64{6, 5}, auditIDs(left))

			page, err := s.ListAudit(ctx, AuditFilter{BeforeID: 6})
			require.NoError(t, err)
			assert.Equal(t, []int64{5}, auditIDs(page))

			require.NoError(t, s.AppendAudit(ctx, fleet.AuditEntry{Actor: "scheduler", Action: "lease.granted"}))

			newest, err := s.ListAudit(ctx, AuditFilter{Limit: 1})
			require.NoError(t, err)
			assert.Equal(t, []int64{7}, auditIDs(newest))
		})
	}
}
