package store

import (
	"context"
	"fmt"
	"math"
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
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			empty, err := s.ListAudit(ctx, AuditFilter{})
			require.NoError(t, err)
			assert.NotNil(t, empty)
			assert.Empty(t, empty)

			require.NoError(t, s.AppendAudit(ctx), "no entries is a no-op")

			detail := map[string]any{
				"before": map[string]any{"paused": false, "soakEpochs": float64(1)},
				"after":  map[string]any{"paused": true, "soakEpochs": float64(2)},
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
