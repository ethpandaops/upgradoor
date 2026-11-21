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

func TestRecordSync(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			validators := &identity.ValidatorRange{Start: 100, End: 200}

			res, err := s.RecordSync(ctx, &HostSync{
				Cert:       testCert("node-1", "bootnode", "rpc"),
				RemoteAddr: "10.0.0.1:1234",
				Request:    reporting(100, 200, imageNeed("a"), imageNeed("b")),
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
				Status:     reporting(100, 200, imageNeed("a"), imageNeed("b")),
			}
			assert.Len(t, res.Host.StatusHash, 32)
			want.StatusHash = res.Host.StatusHash
			assert.Equal(t, want, res.Host)
			assert.Equal(t, uint64(100), res.Host.Weight())
			assert.Contains(t, res.Host.Roles, "bootnode")

			got, err := s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			assert.Equal(t, want, got)

			// Same certificate again: first_seen kept, the rest follows the sync.
			res, err = s.RecordSync(ctx, &HostSync{
				Cert:       testCert("node-1", "bootnode", "rpc"),
				RemoteAddr: "10.0.0.2:999",
				Request:    reporting(100, 200, imageNeed("b"), imageNeed("c"), imageNeed("c")),
				At:         at(60),
			})
			require.NoError(t, err)
			assert.False(t, res.Created)
			assert.Empty(t, res.PreviousCertHash, "unchanged certificate")
			assert.Equal(t, at(0), res.Host.FirstSeen)
			assert.Equal(t, at(60), res.Host.LastSeen)
			assert.Equal(t, "10.0.0.2:999", res.Host.RemoteAddr)
			assert.Len(t, res.Host.Status.Needs, 3, "the status is stored verbatim")

			// A re-issued certificate: new hash, generation and roles. The
			// sync reports no validators any more.
			reissued := testCert("node-1")
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

			// And back to a range.
			res, err = s.RecordSync(ctx, &HostSync{Cert: reissued, Request: reporting(5, 6), At: at(180)})
			require.NoError(t, err)
			assert.Empty(t, res.PreviousCertHash)
			assert.Equal(t, &identity.ValidatorRange{Start: 5, End: 6}, res.Host.Validators)
		})
	}
}

// A sync that restates the stored row writes only the time, the uptime and
// the asked interval; any other difference goes the full way.
func TestRecordSyncConfirms(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			sync := func(sec int, req *protocol.SyncRequest) *SyncResult {
				t.Helper()

				req.SentAt = at(sec)
				req.UptimeSeconds = int64(sec)

				res, err := s.RecordSync(ctx, &HostSync{
					Cert: testCert("node-1"), RemoteAddr: "10.0.0.1:1", Request: req,
					At: at(sec), SyncInterval: time.Duration(sec+1) * time.Second,
				})
				require.NoError(t, err)

				return res
			}

			res := sync(0, reporting(100, 200, imageNeed("a")))
			assert.True(t, res.Changed)
			assert.True(t, res.PreviousSeen.IsZero())

			res = sync(30, reporting(100, 200, imageNeed("a")))
			assert.False(t, res.Changed, "same status apart from its clock")
			assert.Equal(t, at(0), res.PreviousSeen)
			assert.Equal(t, at(30), res.Host.LastSeen)
			assert.Equal(t, int64(30), res.Host.Status.UptimeSeconds)
			assert.True(t, at(30).Equal(res.Host.Status.SentAt))
			assert.Equal(t, 31*time.Second, res.Host.SyncInterval)

			res = sync(60, reporting(100, 200, imageNeed("a"), imageNeed("b")))
			assert.True(t, res.Changed, "a new need")

			res = sync(90, reporting(100, 201, imageNeed("a"), imageNeed("b")))
			assert.True(t, res.Changed, "another range")
			assert.Equal(t, &identity.ValidatorRange{Start: 100, End: 201}, res.Host.Validators)
		})
	}
}

// The generation is the floor below which the API rejects certificates. A
// sync with an older certificate that reached the store anyway does not lower
// it.
func TestRecordSyncNeverLowersGeneration(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			cert := func(generation uint64) *identity.Certificate {
				c := testCert("node-1")
				c.Generation = generation
				c.Hash = fmt.Sprintf("hash-gen-%d", generation)

				return c
			}

			for _, step := range []struct {
				generation, want uint64
			}{
				{generation: 0, want: 0},
				{generation: 2, want: 2},
				{generation: 1, want: 2},
				{generation: 0, want: 2},
				{generation: 2, want: 2},
				{generation: 5, want: 5},
			} {
				res, err := s.RecordSync(ctx, &HostSync{
					Cert: cert(step.generation), Request: testRequest(), At: at(0),
				})
				require.NoError(t, err)
				assert.Equal(t, step.want, res.Host.Generation, "after generation %d", step.generation)

				got, err := s.GetHost(ctx, "node-1")
				require.NoError(t, err)
				assert.Equal(t, step.want, got.Generation)
			}

			// Without a request (identity only) the same holds.
			res, err := s.RecordSync(ctx, &HostSync{Cert: cert(3), At: at(10)})
			require.NoError(t, err)
			assert.Equal(t, uint64(5), res.Host.Generation)
		})
	}
}

// A host that reports a controller hold when the controller sees it for the
// first time keeps the hold: the row is created with the hold wanted.
func TestRecordSyncAdoptsControllerHold(t *testing.T) {
	ctx := context.Background()

	held := func(controller, local bool) *protocol.SyncRequest {
		req := testRequest()
		req.Hold = protocol.Hold{Controller: controller, Local: local}

		return req
	}

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			res, err := s.RecordSync(ctx, &HostSync{
				Cert: testCert("held"), Request: held(true, false), At: at(0),
			})
			require.NoError(t, err)
			assert.True(t, res.Created)
			assert.True(t, res.HoldAdopted)
			assert.True(t, res.Host.HoldWanted)
			assert.Equal(t, HoldReasonAdopted, res.Host.HoldReason)
			assert.Equal(t, "held on the agent before this controller knew the host", res.Host.HoldReason)

			got, err := s.GetHost(ctx, "held")
			require.NoError(t, err)
			assert.True(t, got.HoldWanted)
			assert.Equal(t, HoldReasonAdopted, got.HoldReason)

			// Only the first sight adopts: a person lifts the hold, the agent
			// still reports it until it has the release, and the switch stays
			// off.
			require.NoError(t, setHold(ctx, s, "held", false, ""))

			res, err = s.RecordSync(ctx, &HostSync{
				Cert: testCert("held"), Request: held(true, false), At: at(10),
			})
			require.NoError(t, err)
			assert.False(t, res.Created)
			assert.False(t, res.HoldAdopted)
			assert.False(t, res.Host.HoldWanted)
			assert.Empty(t, res.Host.HoldReason)

			// A local hold file is not the controller's hold.
			res, err = s.RecordSync(ctx, &HostSync{
				Cert: testCert("local"), Request: held(false, true), At: at(0),
			})
			require.NoError(t, err)
			assert.True(t, res.Created)
			assert.False(t, res.HoldAdopted)
			assert.False(t, res.Host.HoldWanted)
			assert.Empty(t, res.Host.HoldReason)

			// No request, no report of a hold.
			res, err = s.RecordSync(ctx, &HostSync{Cert: testCert("bare"), At: at(0)})
			require.NoError(t, err)
			assert.False(t, res.HoldAdopted)
			assert.False(t, res.Host.HoldWanted)

			// A forgotten host that comes back held is adopted again.
			require.NoError(t, s.DeleteHost(ctx, "held"))

			res, err = s.RecordSync(ctx, &HostSync{
				Cert: testCert("held"), Request: held(true, true), At: at(20),
			})
			require.NoError(t, err)
			assert.True(t, res.HoldAdopted)
			assert.True(t, res.Host.HoldWanted)
		})
	}
}

func TestRecordSyncWithoutValidators(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("bootnode-1"), Request: testRequest(), At: at(0)})
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
		Cert:    testCert("node-0"),
		Request: reporting(0, 1),
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
			{"no subject", &HostSync{Cert: testCert(""), Request: testRequest()}},
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

		res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-now"), Request: testRequest()})
		require.NoError(t, err)
		assert.WithinRange(t, res.Host.LastSeen, before, time.Now().Add(time.Second))
		assert.Equal(t, res.Host.FirstSeen, res.Host.LastSeen)
	})

	t.Run("nil request keeps status and needs", func(t *testing.T) {
		_, err := s.RecordSync(ctx, &HostSync{
			Cert: testCert("node-keep"), Request: testRequest(imageNeed("a")), At: at(0),
		})
		require.NoError(t, err)

		res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-keep"), At: at(30)})
		require.NoError(t, err)
		assert.Equal(t, at(30), res.Host.LastSeen)
		assert.Equal(t, testRequest(imageNeed("a")), res.Host.Status)

		// A host first seen without a request has no status.
		res, err = s.RecordSync(ctx, &HostSync{Cert: testCert("node-bare"), At: at(0)})
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

		res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-big"), Request: req, At: at(0)})
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

func TestStatusCache(t *testing.T) {
	var c statusCache

	first, err := c.decode("node-1", `{"uptimeSeconds":1}`)
	require.NoError(t, err)

	again, err := c.decode("node-1", `{"uptimeSeconds":1}`)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	assert.NotSame(t, first, again, "every reader gets a struct of its own")

	changed, err := c.decode("node-1", `{"uptimeSeconds":2}`)
	require.NoError(t, err)
	assert.Equal(t, int64(2), changed.UptimeSeconds)

	_, err = c.decode("node-2", `{`)
	require.Error(t, err)

	c.forget("node-1")
	assert.Empty(t, c.byHost)
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

func TestCommitHoldsSwitch(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	require.ErrorIs(t, setHold(ctx, s, "nope", true, "x"), ErrNotFound)

	addHost(t, s, "node-1", nil)
	addHost(t, s, "node-2", nil)

	require.NoError(t, setHold(ctx, s, "node-1", true, "disk swap"))

	host, err := s.GetHost(ctx, "node-1")
	require.NoError(t, err)
	assert.True(t, host.HoldWanted)
	assert.Equal(t, "disk swap", host.HoldReason)
	assert.True(t, host.Held())

	other, err := s.GetHost(ctx, "node-2")
	require.NoError(t, err)
	assert.False(t, other.HoldWanted)

	// A sync does not touch the switch.
	_, err = s.RecordSync(ctx, &HostSync{Cert: testCert("node-1"), Request: testRequest(), At: at(10)})
	require.NoError(t, err)

	host, err = s.GetHost(ctx, "node-1")
	require.NoError(t, err)
	assert.True(t, host.HoldWanted)

	require.NoError(t, setHold(ctx, s, "node-1", false, ""))

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

			for i, name := range []string{"node-1", "node-2"} {
				_, err := s.RecordSync(ctx, &HostSync{
					Cert: testCert(name), Request: testRequest(imageNeed("a")), At: at(0),
				})
				require.NoError(t, err)
				require.NoError(t, s.SaveInventory(ctx, name, testInventory(10)))
				require.NoError(t, s.SaveSlotParticipation(ctx, []fleet.SlotParticipation{{
					Slot: uint64(i), Proposed: true, Assigned: 1, Included: 1,
					Hosts: []fleet.HostSlotParticipation{{Host: name, Assigned: 1}},
				}}))
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
				"hosts", "inventories", "packages", "upgradable", "slot_participation_hosts",
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
			res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-1"), Request: testRequest(), At: at(500)})
			require.NoError(t, err)
			assert.True(t, res.Created)
			assert.Equal(t, at(500), res.Host.FirstSeen)
			assert.Empty(t, res.Host.InventoryHash)
		})
	}
}

// The range counted for a host follows what the host reports: a range, none,
// or "I do not know", which keeps what was counted.
func TestRecordSyncCountsTheReportedValidators(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	sync := func(t *testing.T, host string, req *protocol.SyncRequest, sec int) *SyncResult {
		t.Helper()

		res, err := s.RecordSync(ctx, &HostSync{Cert: testCert(host), Request: req, At: at(sec)})
		require.NoError(t, err)

		return res
	}

	unknown := func(state string) *protocol.SyncRequest {
		req := testRequest()
		req.Validators = &protocol.Validators{Source: protocol.ValidatorsFromMarker, State: state}

		return req
	}

	res := sync(t, "node-1", reporting(0, 64), 0)
	assert.Nil(t, res.ValidatorsBefore)
	assert.Equal(t, &identity.ValidatorRange{Start: 0, End: 64}, res.Host.Validators)

	// The range changes between two syncs.
	res = sync(t, "node-1", reporting(32, 128), 30)
	assert.Equal(t, &identity.ValidatorRange{Start: 0, End: 64}, res.ValidatorsBefore)
	assert.Equal(t, &identity.ValidatorRange{Start: 32, End: 128}, res.Host.Validators)

	// The marker is gone (keys are being reshuffled) or unreadable: the host
	// keeps its weight. So does an update without a request.
	for i, req := range []*protocol.SyncRequest{
		unknown(protocol.ValidatorsMissing), unknown(protocol.ValidatorsInvalid), nil,
	} {
		res = sync(t, "node-1", req, 60+i)
		assert.Equal(t, &identity.ValidatorRange{Start: 32, End: 128}, res.Host.Validators)
		assert.Equal(t, uint64(96), res.Host.Weight())
	}

	// A range the store would not count is not counted either: the bounds
	// are checked here too, whatever the caller checked.
	bad := reporting(10, 5)
	res = sync(t, "node-1", bad, 70)
	assert.Equal(t, uint64(96), res.Host.Weight())

	// "0-0", and no range at all, are statements: no validators.
	res = sync(t, "node-1", reporting(0, 0), 90)
	assert.Nil(t, res.Host.Validators)
	assert.Equal(t, &identity.ValidatorRange{Start: 32, End: 128}, res.ValidatorsBefore)

	sync(t, "node-1", reporting(32, 128), 100)
	res = sync(t, "node-1", testRequest(), 120)
	assert.Nil(t, res.Host.Validators)

	// A host that never had a range and does not know one has none.
	res = sync(t, "node-new", unknown(protocol.ValidatorsMissing), 120)
	assert.True(t, res.Created)
	assert.Nil(t, res.Host.Validators)
}

// No validator is counted for two hosts, whatever they report.
func TestRecordSyncCountsOverlappingValidatorsOnce(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	sync := func(t *testing.T, host string, start, end uint64, sec int) *SyncResult {
		t.Helper()

		res, err := s.RecordSync(ctx, &HostSync{
			Cert: testCert(host), Request: reporting(start, end), At: at(sec),
		})
		require.NoError(t, err)

		return res
	}

	// counted checks that the ranges of all hosts are disjoint and returns
	// their sum, which is then the number of distinct validators.
	counted := func(t *testing.T) uint64 {
		t.Helper()

		hosts, err := s.ListHosts(ctx)
		require.NoError(t, err)

		var total uint64

		for i, a := range hosts {
			total += a.Weight()

			for _, b := range hosts[i+1:] {
				if a.Validators != nil && b.Validators != nil {
					assert.False(t, a.Validators.Start < b.Validators.End &&
						b.Validators.Start < a.Validators.End,
						"%s %v and %s %v overlap", a.Name, a.Validators, b.Name, b.Validators)
				}
			}
		}

		return total
	}

	sync(t, "node-a", 0, 64, 0)

	// node-b reports half of node-a's validators too.
	res := sync(t, "node-b", 32, 96, 1)
	assert.Equal(t, []string{"node-a"}, res.ValidatorOverlaps)
	assert.Equal(t, &identity.ValidatorRange{Start: 64, End: 96}, res.Host.Validators)
	assert.Equal(t, uint64(96), counted(t), "96 validators exist, not 128")

	// node-a holds its validators; its own report runs into nothing that is
	// counted for another host.
	res = sync(t, "node-a", 0, 64, 2)
	assert.Empty(t, res.ValidatorOverlaps)
	assert.Equal(t, uint64(64), res.Host.Weight())

	// A range inside another host's counts for nothing; one around it
	// counts its larger free part.
	res = sync(t, "node-c", 10, 20, 3)
	assert.Nil(t, res.Host.Validators)
	assert.Equal(t, []string{"node-a"}, res.ValidatorOverlaps)

	res = sync(t, "node-c", 90, 1000, 4)
	assert.Equal(t, &identity.ValidatorRange{Start: 96, End: 1000}, res.Host.Validators)
	assert.Equal(t, []string{"node-b"}, res.ValidatorOverlaps)

	res = sync(t, "node-c", 0, 200, 5)
	assert.Equal(t, &identity.ValidatorRange{Start: 96, End: 200}, res.Host.Validators)
	assert.Equal(t, []string{"node-a", "node-b"}, res.ValidatorOverlaps)
	assert.Equal(t, uint64(200), counted(t))

	// The reshuffle ends: node-a reports its new, smaller range, and node-b
	// counts all of its own at its next sync.
	sync(t, "node-a", 0, 32, 6)
	res = sync(t, "node-b", 32, 96, 7)
	assert.Empty(t, res.ValidatorOverlaps)
	assert.Equal(t, &identity.ValidatorRange{Start: 32, End: 96}, res.Host.Validators)
	assert.Equal(t, uint64(200), counted(t))

	// A host that has been silent for longer than ValidatorClaimTTL gives up
	// the validators another host reports: a host that is gone for good does
	// not keep those of its successor uncounted.
	late := 7 + int(ValidatorClaimTTL/time.Second) + 1

	res = sync(t, "node-d", 0, 32, late)
	assert.Equal(t, []string{"node-a"}, res.ValidatorOverlaps)
	assert.Equal(t, &identity.ValidatorRange{Start: 0, End: 32}, res.Host.Validators)

	gone, err := s.GetHost(ctx, "node-a")
	require.NoError(t, err)
	assert.Nil(t, gone.Validators)
	counted(t)

	// Should it come back, it gets what is left, which is nothing.
	res = sync(t, "node-a", 0, 32, late+1)
	assert.Nil(t, res.Host.Validators)
	assert.Equal(t, []string{"node-d"}, res.ValidatorOverlaps)
}

// setHold sets the controller-side hold switch of one host, as staff does.
func setHold(ctx context.Context, s Store, name string, wanted bool, reason string) error {
	return s.Commit(ctx, &Changes{Holds: []HostHold{{Host: name, Wanted: wanted, Reason: reason}}})
}

// The broken marks are the host's: a commit replaces them, a sync leaves them
// and they go with the host. A host that does not exist has none.
func TestCommitBrokenMarks(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)

	addHost(t, s, "node-1", nil)

	marks := func() map[string]string {
		host, err := s.GetHost(ctx, "node-1")
		require.NoError(t, err)

		return host.Broken
	}

	assert.Nil(t, marks())

	set := map[string]string{"image:sigp/lighthouse:unstable": "lease-1", "packages": "lease-1"}
	require.NoError(t, s.Commit(ctx, &Changes{Broken: []HostBroken{
		{Host: "node-1", Marks: set},
		{Host: "nope", Marks: set},
	}}))
	assert.Equal(t, set, marks())

	_, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-1"), Request: testRequest(), At: at(10)})
	require.NoError(t, err)
	assert.Equal(t, set, marks())

	hosts, err := s.ListHosts(ctx)
	require.NoError(t, err)
	assert.Equal(t, set, hosts[0].Broken)

	require.NoError(t, s.Commit(ctx, &Changes{Broken: []HostBroken{{Host: "node-1"}}}))
	assert.Nil(t, marks())

	// A forgotten host that registers again is a new host.
	require.NoError(t, s.Commit(ctx, &Changes{Broken: []HostBroken{{Host: "node-1", Marks: set}}}))
	require.NoError(t, s.DeleteHost(ctx, "node-1"))
	addHost(t, s, "node-1", nil)
	assert.Nil(t, marks())
}
