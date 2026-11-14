package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// testRollout returns an image rollout created at base+sec seconds with the
// given member hosts (pass them sorted: that is the order they are read in).
func testRollout(id string, state fleet.RolloutState, sec int, hosts ...string) *fleet.Rollout {
	r := &fleet.Rollout{
		ID:        id,
		Kind:      fleet.RolloutImage,
		Key:       "image:ethpandaops/geth:master@sha256:" + id,
		Title:     "geth master -> " + id,
		Image:     "ethpandaops/geth:master",
		Digest:    "sha256:" + id,
		Revision:  "abcdef0",
		State:     state,
		Approved:  true,
		Canary:    "node-1",
		CreatedBy: "scheduler",
		CreatedAt: at(sec),
		UpdatedAt: at(sec + 1),
		Hosts:     make([]fleet.RolloutHost, 0, len(hosts)),
	}

	if state.Terminal() {
		ended := at(sec + 2)
		r.EndedAt = &ended
	}

	for i, host := range hosts {
		r.Hosts = append(r.Hosts, fleet.RolloutHost{
			Host:      host,
			State:     fleet.HostPending,
			Stage:     fleet.Stage(i % 4),
			NeedID:    "need-" + host,
			UpdatedAt: at(sec + 1),
		})
	}

	return r
}

func testLease(id, host string, state fleet.LeaseState, sec int) *fleet.Lease {
	l := &fleet.Lease{
		ID:        id,
		Host:      host,
		State:     state,
		Needs:     []protocol.Need{imageNeed("need-" + host)},
		Rollouts:  map[string]string{"need-" + host: "r1"},
		Weight:    100,
		Charged:   98,
		GrantedBy: "scheduler",
		GrantedAt: at(sec),
		ExpiresAt: at(sec + 1800),
		Operation: &protocol.Operation{},
		Readiness: "waiting for epoch 12",
	}

	if state.Terminal() {
		done, ended := at(sec+60), at(sec+120)
		l.AgentDoneAt, l.EndedAt = &done, &ended
	}

	return l
}

func testDirective(id, host string, state fleet.DirectiveState, sec int) *fleet.Directive {
	return &fleet.Directive{
		Directive: protocol.Directive{
			ID: id, Kind: protocol.DirectiveRoll, Container: "geth", Reason: "manual roll",
		},
		Host:      host,
		State:     state,
		RolloutID: "r1",
		CreatedBy: "alice@example.org",
		CreatedAt: at(sec),
		UpdatedAt: at(sec + 1),
	}
}

func rolloutIDs(rollouts []*fleet.Rollout) []string {
	ids := make([]string, 0, len(rollouts))
	for _, r := range rollouts {
		ids = append(ids, r.ID)
	}

	return ids
}

func leaseIDs(leases []*fleet.Lease) []string {
	ids := make([]string, 0, len(leases))
	for _, l := range leases {
		ids = append(ids, l.ID)
	}

	return ids
}

func directiveIDs(directives []*fleet.Directive) []string {
	ids := make([]string, 0, len(directives))
	for _, d := range directives {
		ids = append(ids, d.ID)
	}

	return ids
}

func TestRollouts(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			_, err := s.GetRollout(ctx, "nope")
			require.ErrorIs(t, err, ErrNotFound)

			empty, err := s.ListRollouts(ctx, RolloutFilter{})
			require.NoError(t, err)
			assert.NotNil(t, empty)
			assert.Empty(t, empty)

			packages := testRollout("r-packages", fleet.RolloutHalted, 30, "node-1", "node-2")
			packages.Kind = fleet.RolloutPackages
			packages.Key = "packages:abc"
			packages.Image, packages.Digest, packages.Revision = "", "", ""
			packages.Packages = []protocol.PackageChange{{Name: "docker-ce", From: "1", To: "2"}}
			packages.Reason = "node-2 failed: \"exit 1\""
			packages.Paused, packages.Force, packages.IgnoreBudget = true, true, true
			packages.Hosts[1].State = fleet.HostFailed
			packages.Hosts[1].LeaseID = "l9"
			packages.Hosts[1].Error = "exit 1"
			packages.Hosts[1].Stage = fleet.StageBootnodes

			all := []*fleet.Rollout{
				testRollout("r-old-done", fleet.RolloutCompleted, 0, "node-1"),
				testRollout("r-active", fleet.RolloutActive, 10, "node-1", "node-2", "node-3"),
				testRollout("r-superseded", fleet.RolloutSuperseded, 20),
				packages,
				testRollout("r-aborted", fleet.RolloutAborted, 40, "node-3"),
				// Same creation time as r-aborted: the ID breaks the tie.
				testRollout("r-zz-same-time", fleet.RolloutActive, 40),
			}
			require.NoError(t, s.Commit(ctx, &Changes{Rollouts: all}))

			for _, want := range all {
				got, getErr := s.GetRollout(ctx, want.ID)
				require.NoError(t, getErr)
				assert.Equal(t, want, got)
			}

			tests := []struct {
				name   string
				filter RolloutFilter
				want   []string
			}{
				{"all, newest first", RolloutFilter{},
					[]string{"r-zz-same-time", "r-aborted", "r-packages", "r-superseded", "r-active", "r-old-done"}},
				{"open only", RolloutFilter{OpenOnly: true},
					[]string{"r-zz-same-time", "r-packages", "r-active"}},
				{"limit", RolloutFilter{Limit: 2}, []string{"r-zz-same-time", "r-aborted"}},
				{"open only with limit", RolloutFilter{OpenOnly: true, Limit: 2},
					[]string{"r-zz-same-time", "r-packages"}},
				{"negative limit means none", RolloutFilter{Limit: -1},
					[]string{"r-zz-same-time", "r-aborted", "r-packages", "r-superseded", "r-active", "r-old-done"}},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					got, getErr := s.ListRollouts(ctx, tt.filter)
					require.NoError(t, getErr)
					assert.Equal(t, tt.want, rolloutIDs(got))
				})
			}

			listed, err := s.ListRollouts(ctx, RolloutFilter{})
			require.NoError(t, err)
			assert.Equal(t, packages, listed[2], "listed rollouts carry their hosts")
			assert.Equal(t, []fleet.RolloutHost{}, listed[3].Hosts, "no hosts is an empty list, not nil")

			// The data column holds the rollout without its hosts.
			var data string

			require.NoError(t, rawDB(t, s).QueryRow(
				`SELECT data FROM rollouts WHERE id = 'r-active'`).Scan(&data))
			assert.NotContains(t, data, "node-2")

			// An update replaces the row and its member rows.
			updated := testRollout("r-active", fleet.RolloutCompleted, 10, "node-2", "node-4")
			updated.Hosts[0].State = fleet.HostDone
			updated.Hosts[1].State = fleet.HostSkipped
			updated.UpdatedAt = at(99)
			require.NoError(t, s.Commit(ctx, &Changes{Rollouts: []*fleet.Rollout{updated}}))

			got, err := s.GetRollout(ctx, "r-active")
			require.NoError(t, err)
			assert.Equal(t, updated, got)
			assert.Nil(t, got.Host("node-1"), "a host no longer listed is gone")
			assert.Equal(t, 6, countRows(t, s, "rollouts"))
			assert.Equal(t, 1+2+0+2+1+0, countRows(t, s, "rollout_hosts"))

			open, err := s.ListRollouts(ctx, RolloutFilter{OpenOnly: true})
			require.NoError(t, err)
			assert.Equal(t, []string{"r-zz-same-time", "r-packages"}, rolloutIDs(open))

			// The caller's rollout is not modified by Commit.
			assert.Len(t, updated.Hosts, 2)
		})
	}
}

// walkTimes calls fn on every time.Time reachable from v and stores what it
// returns. With fill set it first makes every nil pointer point at a new
// value and every slice hold one element, so no time field of the type is
// left out, however deeply nested.
func walkTimes(v reflect.Value, fill bool, fn func(time.Time) time.Time) {
	if v.Type() == reflect.TypeOf(time.Time{}) {
		current, ok := v.Interface().(time.Time)
		if ok {
			v.Set(reflect.ValueOf(fn(current)))
		}

		return
	}

	//nolint:exhaustive // only the kinds that can hold a time matter.
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() && fill {
			v.Set(reflect.New(v.Type().Elem()))
		}

		if !v.IsNil() {
			walkTimes(v.Elem(), fill, fn)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			walkTimes(v.Field(i), fill, fn)
		}
	case reflect.Slice:
		if fill {
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		}

		for i := range v.Len() {
			walkTimes(v.Index(i), fill, fn)
		}
	}
}

// TestCommitTimePrecision: times kept in the JSON columns come back exactly
// like the ones kept in the INTEGER millisecond columns - UTC, whole
// milliseconds - so a reloaded object has one precision throughout.
func TestCommitTimePrecision(t *testing.T) {
	ctx := context.Background()

	// Nanoseconds and a zone other than UTC, a different instant per field.
	zone := time.FixedZone("UTC+2", 2*60*60)
	n := 0
	precise := func(time.Time) time.Time {
		n++

		return time.Date(2026, 3, 1, 14, 0, n, 123_456_789, zone)
	}
	stored := func(in time.Time) time.Time { return in.UTC().Truncate(time.Millisecond) }

	rollout := &fleet.Rollout{}
	walkTimes(reflect.ValueOf(rollout), true, precise)
	rollout.ID = "r1"
	rollout.Hosts[0].Host = "node-1"

	lease := &fleet.Lease{}
	walkTimes(reflect.ValueOf(lease), true, precise)
	lease.ID = "l1"
	lease.Rollouts = map[string]string{}

	directive := &fleet.Directive{}
	walkTimes(reflect.ValueOf(directive), true, precise)
	directive.ID = "d1"

	require.Greater(t, n, 10, "the walk found the time fields")
	require.NotNil(t, rollout.EndedAt)
	require.NotNil(t, lease.Operation)
	require.Equal(t, 123_456_789, lease.Operation.UpdatedAt.Nanosecond())

	// Deep copies of what goes in, to show Commit leaves its input alone.
	rolloutIn, leaseIn, directiveIn := &fleet.Rollout{}, &fleet.Lease{}, &fleet.Directive{}
	for src, dst := range map[any]any{rollout: rolloutIn, lease: leaseIn, directive: directiveIn} {
		raw, err := json.Marshal(src)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, dst))
	}

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			require.NoError(t, s.Commit(ctx, &Changes{
				Rollouts:   []*fleet.Rollout{rollout},
				Leases:     []*fleet.Lease{lease},
				Directives: []*fleet.Directive{directive},
			}))

			gotRollout, err := s.GetRollout(ctx, "r1")
			require.NoError(t, err)

			gotLease, err := s.GetLease(ctx, "l1")
			require.NoError(t, err)

			directives, err := s.ListDirectives(ctx, DirectiveFilter{})
			require.NoError(t, err)
			require.Len(t, directives, 1)

			for _, got := range []any{gotRollout, gotLease, directives[0]} {
				walkTimes(reflect.ValueOf(got), false, func(in time.Time) time.Time {
					assert.Equal(t, time.UTC, in.Location(), "%T: %s", got, in)
					assert.Zero(t, in.Nanosecond()%int(time.Millisecond), "%T: %s", got, in)

					return in
				})
			}

			// Apart from the precision, nothing changed.
			wantRollout, wantLease, wantDirective := &fleet.Rollout{}, &fleet.Lease{}, &fleet.Directive{}
			for src, dst := range map[any]any{
				rollout: wantRollout, lease: wantLease, directive: wantDirective,
			} {
				raw, marshalErr := json.Marshal(src)
				require.NoError(t, marshalErr)
				require.NoError(t, json.Unmarshal(raw, dst))
				walkTimes(reflect.ValueOf(dst), false, stored)
			}

			assert.Equal(t, wantRollout, gotRollout)
			assert.Equal(t, wantLease, gotLease)
			assert.Equal(t, wantDirective, directives[0])

			// Times set from one instant agree after a reload, wherever
			// they are kept.
			now := time.Now()
			same := testRollout("r-now", fleet.RolloutActive, 0, "node-1")
			same.UpdatedAt, same.Hosts[0].UpdatedAt = now, now
			require.NoError(t, s.Commit(ctx, &Changes{Rollouts: []*fleet.Rollout{same}}))

			reloaded, err := s.GetRollout(ctx, "r-now")
			require.NoError(t, err)
			assert.Equal(t, reloaded.UpdatedAt, reloaded.Hosts[0].UpdatedAt)
			assert.Equal(t, now.UTC().Truncate(time.Millisecond), reloaded.UpdatedAt)

			// The caller's objects keep their own precision.
			assert.Equal(t, rolloutIn.CreatedAt.Nanosecond(), rollout.CreatedAt.Nanosecond())
			assert.Equal(t, 123_456_789, rollout.EndedAt.Nanosecond())
			assert.Equal(t, 123_456_789, lease.GrantedAt.Nanosecond())
			assert.Equal(t, 123_456_789, lease.AgentDoneAt.Nanosecond())
			assert.Equal(t, 123_456_789, lease.Operation.StartedAt.Nanosecond())
			assert.Equal(t, leaseIn.ExpiresAt.Nanosecond(), lease.ExpiresAt.Nanosecond())
			assert.Equal(t, directiveIn.CreatedAt.Nanosecond(), directive.CreatedAt.Nanosecond())
		})
	}
}

func TestRolloutWithManyHosts(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	hosts := make([]string, 0, 3*insertBatchRows)
	for i := range 3*insertBatchRows + 1 {
		hosts = append(hosts, fmt.Sprintf("node-%04d", i))
	}

	want := testRollout("r-big", fleet.RolloutActive, 0, hosts...)
	require.NoError(t, s.Commit(ctx, &Changes{Rollouts: []*fleet.Rollout{want}}))

	got, err := s.GetRollout(ctx, "r-big")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestLeases(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			_, err := s.GetLease(ctx, "nope")
			require.ErrorIs(t, err, ErrNotFound)

			empty, err := s.ListLeases(ctx, LeaseFilter{})
			require.NoError(t, err)
			assert.NotNil(t, empty)
			assert.Empty(t, empty)

			failed := testLease("l-failed", "node-2", fleet.LeaseFailed, 20)
			failed.Error = "container did not become healthy"
			failed.Force = true
			failed.Operation = nil

			all := []*fleet.Lease{
				testLease("l-released", "node-1", fleet.LeaseReleased, 0),
				testLease("l-granted", "node-1", fleet.LeaseGranted, 10),
				failed,
				testLease("l-running", "node-2", fleet.LeaseRunning, 30),
				testLease("l-verifying", "node-3", fleet.LeaseVerifying, 40),
				testLease("l-expired", "node-3", fleet.LeaseExpired, 50),
			}
			require.NoError(t, s.Commit(ctx, &Changes{Leases: all}))

			for _, want := range all {
				got, getErr := s.GetLease(ctx, want.ID)
				require.NoError(t, getErr)
				assert.Equal(t, want, got)
			}

			tests := []struct {
				name   string
				filter LeaseFilter
				want   []string
			}{
				{"all, newest first", LeaseFilter{},
					[]string{"l-expired", "l-verifying", "l-running", "l-failed", "l-granted", "l-released"}},
				{"open only", LeaseFilter{OpenOnly: true}, []string{"l-verifying", "l-running", "l-granted"}},
				{"one host", LeaseFilter{Host: "node-1"}, []string{"l-granted", "l-released"}},
				{"one host, open only", LeaseFilter{Host: "node-3", OpenOnly: true}, []string{"l-verifying"}},
				{"limit", LeaseFilter{Limit: 2}, []string{"l-expired", "l-verifying"}},
				{"host with limit", LeaseFilter{Host: "node-2", Limit: 1}, []string{"l-running"}},
				{"unknown host", LeaseFilter{Host: "nope"}, []string{}},
				{"host is exact", LeaseFilter{Host: "node-%"}, []string{}},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					got, getErr := s.ListLeases(ctx, tt.filter)
					require.NoError(t, getErr)
					assert.Equal(t, tt.want, leaseIDs(got))
				})
			}

			// Upsert: the lease moves on.
			moved := testLease("l-granted", "node-1", fleet.LeaseReleased, 10)
			require.NoError(t, s.Commit(ctx, &Changes{Leases: []*fleet.Lease{moved}}))

			got, err := s.GetLease(ctx, "l-granted")
			require.NoError(t, err)
			assert.Equal(t, moved, got)
			assert.Equal(t, 6, countRows(t, s, "leases"))

			open, err := s.ListLeases(ctx, LeaseFilter{Host: "node-1", OpenOnly: true})
			require.NoError(t, err)
			assert.Empty(t, open)
		})
	}
}

func TestDirectives(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			empty, err := s.ListDirectives(ctx, DirectiveFilter{})
			require.NoError(t, err)
			assert.NotNil(t, empty)
			assert.Empty(t, empty)

			rejected := testDirective("d-rejected", "node-2", fleet.DirectiveRejected, 20)
			rejected.Error = "local hold"

			hold := testDirective("d-hold", "node-2", fleet.DirectivePending, 30)
			hold.Directive = protocol.Directive{ID: "d-hold", Kind: protocol.DirectiveHold}
			hold.RolloutID = ""

			all := []*fleet.Directive{
				hold,
				testDirective("d-pending", "node-1", fleet.DirectivePending, 0),
				testDirective("d-accepted", "node-1", fleet.DirectiveAccepted, 10),
				rejected,
				testDirective("d-cancelled", "node-3", fleet.DirectiveCancelled, 40),
				// Same creation time as d-pending: the ID breaks the tie.
				testDirective("d-a-same-time", "node-3", fleet.DirectivePending, 0),
			}
			require.NoError(t, s.Commit(ctx, &Changes{Directives: all}))

			tests := []struct {
				name   string
				filter DirectiveFilter
				want   []string
			}{
				{"all, oldest first", DirectiveFilter{},
					[]string{"d-a-same-time", "d-pending", "d-accepted", "d-rejected", "d-hold", "d-cancelled"}},
				{"pending only", DirectiveFilter{PendingOnly: true}, []string{"d-a-same-time", "d-pending", "d-hold"}},
				{"one host", DirectiveFilter{Host: "node-2"}, []string{"d-rejected", "d-hold"}},
				{"one host, pending only", DirectiveFilter{Host: "node-2", PendingOnly: true}, []string{"d-hold"}},
				{"unknown host", DirectiveFilter{Host: "nope"}, []string{}},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					got, getErr := s.ListDirectives(ctx, tt.filter)
					require.NoError(t, getErr)
					assert.Equal(t, tt.want, directiveIDs(got))
				})
			}

			got, err := s.ListDirectives(ctx, DirectiveFilter{Host: "node-2"})
			require.NoError(t, err)
			assert.Equal(t, []*fleet.Directive{rejected, hold}, got, "every field round-trips")

			// Upsert: acknowledged by the agent.
			acked := testDirective("d-pending", "node-1", fleet.DirectiveAccepted, 0)
			acked.UpdatedAt = at(77)
			require.NoError(t, s.Commit(ctx, &Changes{Directives: []*fleet.Directive{acked}}))

			got, err = s.ListDirectives(ctx, DirectiveFilter{Host: "node-1"})
			require.NoError(t, err)
			assert.Equal(t, []*fleet.Directive{acked, testDirective("d-accepted", "node-1", fleet.DirectiveAccepted, 10)}, got)
			assert.Equal(t, 6, countRows(t, s, "directives"))
		})
	}
}

func TestCommit(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			require.NoError(t, s.Commit(ctx, nil))
			require.NoError(t, s.Commit(ctx, &Changes{}))

			changes := &Changes{
				Rollouts:   []*fleet.Rollout{testRollout("r1", fleet.RolloutActive, 0, "node-1")},
				Leases:     []*fleet.Lease{testLease("l1", "node-1", fleet.LeaseGranted, 0)},
				Directives: []*fleet.Directive{testDirective("d1", "node-1", fleet.DirectivePending, 0)},
				Audit: []fleet.AuditEntry{
					{Time: at(5), Actor: "scheduler", Action: "rollout.created", Target: "r1"},
					{Actor: "scheduler", Action: "lease.granted", Target: "node-1",
						Detail: map[string]any{"lease": "l1"}},
				},
			}
			require.NoError(t, s.Commit(ctx, changes))

			assert.Equal(t, int64(1), changes.Audit[0].ID, "IDs are reported back")
			assert.Equal(t, int64(2), changes.Audit[1].ID)
			assert.Equal(t, at(5), changes.Audit[0].Time)
			assert.WithinDuration(t, time.Now(), changes.Audit[1].Time, time.Minute, "a zero time is set to now")

			audit, err := s.ListAudit(ctx, AuditFilter{})
			require.NoError(t, err)
			require.Len(t, audit, 2)
			assert.Equal(t, "lease.granted", audit[0].Action)
			assert.Equal(t, map[string]any{"lease": "l1"}, audit[0].Detail)

			for table, want := range map[string]int{
				"rollouts": 1, "rollout_hosts": 1, "leases": 1, "directives": 1, "audit": 2,
			} {
				assert.Equal(t, want, countRows(t, s, table), table)
			}
		})
	}
}

// TestCommitAtomicity puts one failing element into an otherwise valid set
// of changes, at every position, and checks that nothing at all was written:
// neither new rows nor updates of existing ones.
func TestCommitAtomicity(t *testing.T) {
	ctx := context.Background()

	duplicateHosts := testRollout("r-dup", fleet.RolloutActive, 5, "node-1", "node-1")

	unencodable := testLease("l-bad", "node-1", fleet.LeaseGranted, 5)
	unencodable.Operation = nil
	unencodable.Readiness = "ok"

	tests := []struct {
		name    string
		poison  func(c *Changes)
		wantErr string
	}{
		{
			name:    "rollout without id",
			poison:  func(c *Changes) { c.Rollouts = append(c.Rollouts, &fleet.Rollout{State: fleet.RolloutActive}) },
			wantErr: "rollout has no id",
		},
		{
			name:    "nil rollout",
			poison:  func(c *Changes) { c.Rollouts = append(c.Rollouts, nil) },
			wantErr: "rollout is nil",
		},
		{
			name:    "rollout that violates a constraint in the database",
			poison:  func(c *Changes) { c.Rollouts = append(c.Rollouts, duplicateHosts) },
			wantErr: "r-dup",
		},
		{
			name:    "lease without id",
			poison:  func(c *Changes) { c.Leases = append(c.Leases, &fleet.Lease{Host: "node-1"}) },
			wantErr: "lease has no id",
		},
		{
			name:    "nil lease",
			poison:  func(c *Changes) { c.Leases = append(c.Leases, nil) },
			wantErr: "lease is nil",
		},
		{
			name:    "directive without id",
			poison:  func(c *Changes) { c.Directives = append(c.Directives, &fleet.Directive{Host: "node-1"}) },
			wantErr: "directive has no id",
		},
		{
			name:    "nil directive",
			poison:  func(c *Changes) { c.Directives = append(c.Directives, nil) },
			wantErr: "directive is nil",
		},
		{
			name: "audit entry that cannot be encoded (the very last write)",
			poison: func(c *Changes) {
				c.Audit = append(c.Audit, fleet.AuditEntry{
					Actor: "scheduler", Action: "bad", Detail: map[string]any{"x": math.NaN()},
				})
			},
			wantErr: "encode detail",
		},
	}

	for _, tt := range tests {
		for _, backend := range backends {
			t.Run(tt.name+"/"+backend.name, func(t *testing.T) {
				s := backend.open(t)

				// Existing state that the failing commit tries to change.
				before := &Changes{
					Rollouts:   []*fleet.Rollout{testRollout("r1", fleet.RolloutActive, 0, "node-1", "node-2")},
					Leases:     []*fleet.Lease{testLease("l1", "node-1", fleet.LeaseGranted, 0)},
					Directives: []*fleet.Directive{testDirective("d1", "node-1", fleet.DirectivePending, 0)},
					Audit:      []fleet.AuditEntry{{Time: at(0), Actor: "scheduler", Action: "rollout.created", Target: "r1"}},
				}
				require.NoError(t, s.Commit(ctx, before))

				changes := &Changes{
					Rollouts: []*fleet.Rollout{
						testRollout("r1", fleet.RolloutHalted, 0, "node-1"),
						testRollout("r2", fleet.RolloutActive, 1, "node-3"),
					},
					Leases: []*fleet.Lease{
						testLease("l1", "node-1", fleet.LeaseFailed, 0),
						testLease("l2", "node-3", fleet.LeaseGranted, 1),
					},
					Directives: []*fleet.Directive{
						testDirective("d1", "node-1", fleet.DirectiveAccepted, 0),
						testDirective("d2", "node-3", fleet.DirectivePending, 1),
					},
					Audit: []fleet.AuditEntry{{Actor: "scheduler", Action: "lease.failed", Target: "node-1"}},
				}
				tt.poison(changes)

				err := s.Commit(ctx, changes)
				require.ErrorContains(t, err, tt.wantErr)

				for _, entry := range changes.Audit {
					assert.Zero(t, entry.ID, "no audit ID is reported for a failed commit")
				}

				rollouts, err := s.ListRollouts(ctx, RolloutFilter{})
				require.NoError(t, err)
				assert.Equal(t, before.Rollouts, rollouts)

				leases, err := s.ListLeases(ctx, LeaseFilter{})
				require.NoError(t, err)
				assert.Equal(t, before.Leases, leases)

				directives, err := s.ListDirectives(ctx, DirectiveFilter{})
				require.NoError(t, err)
				assert.Equal(t, before.Directives, directives)

				audit, err := s.ListAudit(ctx, AuditFilter{})
				require.NoError(t, err)
				require.Len(t, audit, 1)
				assert.Equal(t, "rollout.created", audit[0].Action)

				assert.Equal(t, 2, countRows(t, s, "rollout_hosts"))

				// The store is not left in a broken transaction: the same
				// changes without the poison go through, and the audit IDs
				// continue without a gap that a half-commit would explain.
				require.NoError(t, s.Commit(ctx, &Changes{
					Rollouts: []*fleet.Rollout{testRollout("r2", fleet.RolloutActive, 1, "node-3")},
					Audit:    []fleet.AuditEntry{{Actor: "scheduler", Action: "rollout.created", Target: "r2"}},
				}))
				assert.Equal(t, 2, countRows(t, s, "rollouts"))
			})
		}
	}
}
