package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// storedNeeds reads the needs rows of a host: id -> first_seen.
func storedNeeds(t *testing.T, s Store, host string) map[string]time.Time {
	t.Helper()

	rows, err := rawDB(t, s).Query(`SELECT id, kind, detail, first_seen FROM needs WHERE host = ?`, host)
	require.NoError(t, err)

	defer func() { require.NoError(t, rows.Close()) }()

	needs := make(map[string]time.Time, 4)

	for rows.Next() {
		var (
			id, kind, detail string
			firstSeen        int64
		)

		require.NoError(t, rows.Scan(&id, &kind, &detail, &firstSeen))
		assert.Equal(t, string(protocol.NeedImage), kind)
		assert.Contains(t, detail, `"id":"`+id+`"`)

		needs[id] = fromMillis(firstSeen)
	}

	require.NoError(t, rows.Err())

	return needs
}

func TestRecordSync(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			validators := &identity.ValidatorRange{Start: 100, End: 200}

			res, err := s.RecordSync(ctx, &HostSync{
				Cert:       testCert("node-1", validators, "bootnode", "rpc"),
				RemoteAddr: "10.0.0.1:1234",
				Request:    testRequest(imageNeed("a"), imageNeed("b")),
				At:         at(0),
			})
			require.NoError(t, err)
			assert.True(t, res.Created)
			assert.Empty(t, res.PreviousCertHash)

			want := &fleet.Host{
				Name:       "node-1",
				CertHash:   "hash-node-1",
				Generation: 1,
				Validators: validators,
				Roles:      []string{"bootnode", "rpc"},
				FirstSeen:  at(0),
				LastSeen:   at(0),
				RemoteAddr: "10.0.0.1:1234",
				Status:     testRequest(imageNeed("a"), imageNeed("b")),
			}
			assert.Equal(t, want, res.Host)
			assert.Equal(t, uint64(100), res.Host.Weight())
			assert.True(t, res.Host.IsBootnode())

			got, err := s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, map[string]time.Time{"a": at(0), "b": at(0)}, storedNeeds(t, s, "node-1"))

			// Same certificate again: first_seen kept, the rest follows the sync.
			res, err = s.RecordSync(ctx, &HostSync{
				Cert:       testCert("node-1", validators, "bootnode", "rpc"),
				RemoteAddr: "10.0.0.2:999",
				Request:    testRequest(imageNeed("b"), imageNeed("c"), imageNeed("c")),
				At:         at(60),
			})
			require.NoError(t, err)
			assert.False(t, res.Created)
			assert.Empty(t, res.PreviousCertHash, "unchanged certificate")
			assert.Equal(t, at(0), res.Host.FirstSeen)
			assert.Equal(t, at(60), res.Host.LastSeen)
			assert.Equal(t, "10.0.0.2:999", res.Host.RemoteAddr)
			assert.Len(t, res.Host.Status.Needs, 3, "the status is stored verbatim")
			assert.Equal(t, map[string]time.Time{"b": at(0), "c": at(60)}, storedNeeds(t, s, "node-1"),
				"a persisting need keeps first_seen, a gone one is removed, a repeated ID is stored once")

			// A re-issued certificate: new hash, generation, range and roles.
			reissued := testCert("node-1", nil)
			reissued.Hash = "hash-2"
			reissued.Generation = 2

			res, err = s.RecordSync(ctx, &HostSync{Cert: reissued, Request: testRequest(), At: at(120)})
			require.NoError(t, err)
			assert.False(t, res.Created)
			assert.Equal(t, "hash-node-1", res.PreviousCertHash)
			assert.Equal(t, "hash-2", res.Host.CertHash)
			assert.Equal(t, uint64(2), res.Host.Generation)
			assert.Nil(t, res.Host.Validators)
			assert.Equal(t, []string{}, res.Host.Roles)
			assert.Empty(t, res.Host.RemoteAddr)
			assert.Equal(t, at(0), res.Host.FirstSeen)
			assert.Empty(t, storedNeeds(t, s, "node-1"), "no needs reported, none stored")

			// And back to a range.
			reissued.Validators = &identity.ValidatorRange{Start: 5, End: 6}

			res, err = s.RecordSync(ctx, &HostSync{Cert: reissued, Request: testRequest(), At: at(180)})
			require.NoError(t, err)
			assert.Empty(t, res.PreviousCertHash)
			assert.Equal(t, &identity.ValidatorRange{Start: 5, End: 6}, res.Host.Validators)
		})
	}
}

func TestRecordSyncWithoutValidators(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("bootnode-1", nil), Request: testRequest(), At: at(0)})
	require.NoError(t, err)
	assert.Nil(t, res.Host.Validators)
	assert.Zero(t, res.Host.Weight())
	assert.Equal(t, []string{}, res.Host.Roles, "roles are never nil")

	var start, end any

	require.NoError(t, rawDB(t, s).QueryRow(
		`SELECT validators_start, validators_end FROM hosts WHERE name = 'bootnode-1'`).Scan(&start, &end))
	assert.Nil(t, start, "stored as NULL")
	assert.Nil(t, end, "stored as NULL")

	got, err := s.GetHost(ctx, "bootnode-1")
	require.NoError(t, err)
	assert.Nil(t, got.Validators)

	// A range that starts at validator 0 is not confused with "none".
	res, err = s.RecordSync(ctx, &HostSync{
		Cert:    testCert("node-0", &identity.ValidatorRange{Start: 0, End: 1}),
		Request: testRequest(),
		At:      at(0),
	})
	require.NoError(t, err)
	assert.Equal(t, &identity.ValidatorRange{Start: 0, End: 1}, res.Host.Validators)
}

func TestRecordSyncEdgeCases(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	t.Run("invalid input", func(t *testing.T) {
		tests := []struct {
			name string
			in   *HostSync
		}{
			{"nil", nil},
			{"no certificate", &HostSync{Request: testRequest()}},
			{"no subject", &HostSync{Cert: testCert("", nil), Request: testRequest()}},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := s.RecordSync(ctx, tt.in)
				require.Error(t, err)
			})
		}

		assert.Zero(t, countRows(t, s, "hosts"))
	})

	t.Run("zero time means now", func(t *testing.T) {
		before := time.Now().Add(-time.Second)

		res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-now", nil), Request: testRequest()})
		require.NoError(t, err)
		assert.WithinRange(t, res.Host.LastSeen, before, time.Now().Add(time.Second))
		assert.Equal(t, res.Host.FirstSeen, res.Host.LastSeen)
	})

	t.Run("nil request keeps status and needs", func(t *testing.T) {
		_, err := s.RecordSync(ctx, &HostSync{
			Cert: testCert("node-keep", nil), Request: testRequest(imageNeed("a")), At: at(0),
		})
		require.NoError(t, err)

		res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-keep", nil), At: at(30)})
		require.NoError(t, err)
		assert.Equal(t, at(30), res.Host.LastSeen)
		assert.Equal(t, testRequest(imageNeed("a")), res.Host.Status)
		assert.Equal(t, map[string]time.Time{"a": at(0)}, storedNeeds(t, s, "node-keep"))

		// A host first seen without a request has no status.
		res, err = s.RecordSync(ctx, &HostSync{Cert: testCert("node-bare", nil), At: at(0)})
		require.NoError(t, err)
		assert.True(t, res.Created)
		assert.Nil(t, res.Host.Status)
	})

	t.Run("large status", func(t *testing.T) {
		req := testRequest()
		req.Containers = make([]protocol.Container, 0, 2000)

		for i := range 2000 {
			req.Containers = append(req.Containers, protocol.Container{
				Name:      fmt.Sprintf("container-%04d", i),
				ID:        strings.Repeat("f", 64),
				Image:     "registry.example.org/some/long/path/image:" + strings.Repeat("t", 400),
				ImageID:   "sha256:" + strings.Repeat("a", 64),
				Digest:    "sha256:" + strings.Repeat("b", 64),
				State:     "running \"quoted\" ü世界 \x00 'x' -- ; DROP TABLE hosts",
				StartedAt: at(i),
			})
		}

		res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-big", nil), Request: req, At: at(0)})
		require.NoError(t, err)
		assert.Equal(t, req, res.Host.Status)

		var size int

		require.NoError(t, rawDB(t, s).QueryRow(
			`SELECT length(status) FROM hosts WHERE name = 'node-big'`).Scan(&size))
		assert.Greater(t, size, protocol.MaxSyncBody, "larger than any real sync body")

		got, err := s.GetHost(ctx, "node-big")
		require.NoError(t, err)
		assert.Equal(t, req, got.Status)
	})
}

func TestGetAndListHosts(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			_, err := s.GetHost(ctx, "nope")
			require.ErrorIs(t, err, ErrNotFound)

			hosts, err := s.ListHosts(ctx)
			require.NoError(t, err)
			assert.NotNil(t, hosts)
			assert.Empty(t, hosts)

			for _, name := range []string{"node-b", "node-c", "node-a", "Node-Z"} {
				addHost(t, s, name, nil)
			}

			hosts, err = s.ListHosts(ctx)
			require.NoError(t, err)

			names := make([]string, 0, len(hosts))
			for _, h := range hosts {
				names = append(names, h.Name)
			}

			assert.Equal(t, []string{"Node-Z", "node-a", "node-b", "node-c"}, names)

			// Names are exact: no case folding, no wildcard.
			_, err = s.GetHost(ctx, "NODE-A")
			require.ErrorIs(t, err, ErrNotFound)
			_, err = s.GetHost(ctx, "node-%")
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

func TestSetHostHold(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	require.ErrorIs(t, s.SetHostHold(ctx, "nope", true, "x"), ErrNotFound)

	addHost(t, s, "node-1", nil)
	addHost(t, s, "node-2", nil)

	require.NoError(t, s.SetHostHold(ctx, "node-1", true, "disk swap"))

	host, err := s.GetHost(ctx, "node-1")
	require.NoError(t, err)
	assert.True(t, host.HoldWanted)
	assert.Equal(t, "disk swap", host.HoldReason)
	assert.True(t, host.Held())

	other, err := s.GetHost(ctx, "node-2")
	require.NoError(t, err)
	assert.False(t, other.HoldWanted)

	// A sync does not touch the switch.
	_, err = s.RecordSync(ctx, &HostSync{Cert: testCert("node-1", nil), Request: testRequest(), At: at(10)})
	require.NoError(t, err)

	host, err = s.GetHost(ctx, "node-1")
	require.NoError(t, err)
	assert.True(t, host.HoldWanted)

	require.NoError(t, s.SetHostHold(ctx, "node-1", false, ""))

	host, err = s.GetHost(ctx, "node-1")
	require.NoError(t, err)
	assert.False(t, host.HoldWanted)
	assert.Empty(t, host.HoldReason)
}

func TestDeleteHost(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			require.ErrorIs(t, s.DeleteHost(ctx, "nope"), ErrNotFound)

			for _, name := range []string{"node-1", "node-2"} {
				_, err := s.RecordSync(ctx, &HostSync{
					Cert: testCert(name, nil), Request: testRequest(imageNeed("a")), At: at(0),
				})
				require.NoError(t, err)
				require.NoError(t, s.SaveInventory(ctx, name, testInventory(10)))
				require.NoError(t, s.SaveLiveness(ctx, []fleet.Liveness{{Host: name, Epoch: 1, Live: 1, Total: 1}}))
				require.NoError(t, s.Commit(ctx, &Changes{
					Rollouts:   []*fleet.Rollout{testRollout("r-"+name, fleet.RolloutCompleted, 0, name)},
					Leases:     []*fleet.Lease{testLease("l-"+name, name, fleet.LeaseReleased, 0)},
					Directives: []*fleet.Directive{testDirective("d-"+name, name, fleet.DirectiveAccepted, 0)},
					Audit:      []fleet.AuditEntry{{Actor: "scheduler", Action: "lease.released", Target: name}},
				}))
			}

			require.NoError(t, s.DeleteHost(ctx, "node-1"))

			_, err := s.GetHost(ctx, "node-1")
			require.ErrorIs(t, err, ErrNotFound)
			_, err = s.GetInventory(ctx, "node-1")
			require.ErrorIs(t, err, ErrNotFound)
			require.ErrorIs(t, s.DeleteHost(ctx, "node-1"), ErrNotFound)

			for _, table := range []string{
				"hosts", "inventories", "packages", "upgradable", "needs", "validator_liveness",
			} {
				var mine, others int

				column := "host"
				if table == "hosts" {
					column = "name"
				}

				require.NoError(t, rawDB(t, s).QueryRow(
					`SELECT COUNT(*) FROM `+table+` WHERE `+column+` = 'node-1'`).Scan(&mine))
				require.NoError(t, rawDB(t, s).QueryRow(
					`SELECT COUNT(*) FROM `+table+` WHERE `+column+` = 'node-2'`).Scan(&others))
				assert.Zero(t, mine, "%s rows of the deleted host", table)
				assert.NotZero(t, others, "%s rows of the other host", table)
			}

			// History stays.
			rollout, err := s.GetRollout(ctx, "r-node-1")
			require.NoError(t, err)
			assert.Equal(t, testRollout("r-node-1", fleet.RolloutCompleted, 0, "node-1"), rollout)

			lease, err := s.GetLease(ctx, "l-node-1")
			require.NoError(t, err)
			assert.Equal(t, "node-1", lease.Host)

			directives, err := s.ListDirectives(ctx, DirectiveFilter{Host: "node-1"})
			require.NoError(t, err)
			assert.Len(t, directives, 1)

			audit, err := s.ListAudit(ctx, AuditFilter{Target: "node-1"})
			require.NoError(t, err)
			assert.Len(t, audit, 1)

			// The host can register again, as new.
			res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-1", nil), Request: testRequest(), At: at(500)})
			require.NoError(t, err)
			assert.True(t, res.Created)
			assert.Equal(t, at(500), res.Host.FirstSeen)
			assert.Empty(t, res.Host.InventoryHash)
		})
	}
}
