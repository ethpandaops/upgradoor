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
	t.Parallel()

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			s := backend.open(t)

			validators := []identity.ValidatorRange{{Start: 100, End: 200}}
			assign(t, s, fleet.HostRange{Host: "node-1", ValidatorRange: validators[0]})

			res, err := s.RecordSync(ctx, &HostSync{
				Cert:       testCert("node-1"),
				RemoteAddr: "10.0.0.1:1234",
				Request:    withRoles(testRequest(imageNeed("a"), imageNeed("b")), "bootnode", "rpc"),
				At:         at(0),
			})
			require.NoError(t, err)
			assert.True(t, res.Created)
			assert.Empty(t, res.PreviousCertHash)

			want := &fleet.Host{
				Name:       "node-1",
				CertHash:   "hash-node-1",
				Validators: validators,
				Roles:      []string{"bootnode", "rpc"},
				FirstSeen:  at(0),
				LastSeen:   at(0),
				RemoteAddr: "10.0.0.1:1234",
				Status:     withRoles(testRequest(imageNeed("a"), imageNeed("b")), "bootnode", "rpc"),
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
				Cert:       testCert("node-1"),
				RemoteAddr: "10.0.0.2:999",
				Request: withRoles(testRequest(imageNeed("b"), imageNeed("c"), imageNeed("c")),
					"bootnode", "rpc"),
				At: at(60),
			})
			require.NoError(t, err)
			assert.False(t, res.Created)
			assert.Empty(t, res.PreviousCertHash, "unchanged certificate")
			assert.Equal(t, at(0), res.Host.FirstSeen)
			assert.Equal(t, at(60), res.Host.LastSeen)
			assert.Equal(t, "10.0.0.2:999", res.Host.RemoteAddr)
			assert.Len(t, res.Host.Status.Needs, 3, "the status is stored verbatim")

			// A re-issued certificate: a new hash. The sync reports no
			// roles any more; the validators stay with the name.
			reissued := testCert("node-1")
			reissued.Hash = "hash-2"

			res, err = s.RecordSync(ctx, &HostSync{Cert: reissued, Request: testRequest(), At: at(120)})
			require.NoError(t, err)
			assert.False(t, res.Created)
			assert.Equal(t, "hash-node-1", res.PreviousCertHash)
			assert.Equal(t, "hash-2", res.Host.CertHash)
			assert.Equal(t, validators, res.Host.Validators)
			assert.Equal(t, []string{}, res.Host.Roles)
			assert.Empty(t, res.Host.RemoteAddr)
			assert.Equal(t, at(0), res.Host.FirstSeen)
		})
	}
}

// A sync that restates the stored row writes only the time, the status and
// the asked interval; any other difference goes the full way.
func TestRecordSyncConfirms(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

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

			res := sync(0, testRequest(imageNeed("a")))
			assert.True(t, res.Changed)
			assert.True(t, res.PreviousSeen.IsZero())

			res = sync(30, testRequest(imageNeed("a")))
			assert.False(t, res.Changed, "same status apart from its clock")
			assert.Equal(t, at(0), res.PreviousSeen)
			assert.Equal(t, at(30), res.Host.LastSeen)
			assert.Equal(t, int64(30), res.Host.Status.UptimeSeconds)
			assert.True(t, at(30).Equal(res.Host.Status.SentAt))
			assert.Equal(t, 31*time.Second, res.Host.SyncInterval)

			// The heads, the sync distance and the peer counts move on a
			// healthy node: stored, but not a change.
			moved := testRequest(imageNeed("a"))
			moved.Node.EL.Head, moved.Node.EL.Peers = 101, 13
			moved.Node.CL.HeadSlot, moved.Node.CL.SyncDistance, moved.Node.CL.Peers = 3201, 1, 29
			res = sync(40, moved)
			assert.False(t, res.Changed, "only the probes' heads and peer counts moved")
			assert.Equal(t, moved.Node, res.Host.Status.Node)

			stored, err := s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			assert.Equal(t, moved.Node, stored.Status.Node)

			for name, change := range map[string]func(n *protocol.NodeHealth){
				"EL syncing":       func(n *protocol.NodeHealth) { n.EL.Syncing = true },
				"CL not reachable": func(n *protocol.NodeHealth) { n.CL.Reachable = false },
				"CL EL offline":    func(n *protocol.NodeHealth) { n.CL.ELOffline = true },
				"EL without peers": func(n *protocol.NodeHealth) { n.EL.Peers = 0 },
			} {
				req := testRequest(imageNeed("a"))
				change(&req.Node)
				assert.True(t, sync(45, req).Changed, name)
				assert.True(t, sync(50, testRequest(imageNeed("a"))).Changed, name+" ends")
			}

			res = sync(60, testRequest(imageNeed("a"), imageNeed("b")))
			assert.True(t, res.Changed, "a new need")
		})
	}
}

// A host that reports a controller hold when the controller sees it for the
// first time keeps the hold: the row is created with the hold wanted.
func TestRecordSyncAdoptsControllerHold(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	held := func(controller, local bool) *protocol.SyncRequest {
		req := testRequest()
		req.Hold = protocol.Hold{Controller: controller, Local: local}

		return req
	}

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

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

			// A local hold is not the controller's hold.
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

	t.Run("nil request keeps status, needs and roles", func(t *testing.T) {
		_, err := s.RecordSync(ctx, &HostSync{
			Cert: testCert("node-keep"), Request: withRoles(testRequest(imageNeed("a")), "rpc"), At: at(0),
		})
		require.NoError(t, err)

		res, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-keep"), At: at(30)})
		require.NoError(t, err)
		assert.Equal(t, at(30), res.Host.LastSeen)
		assert.Equal(t, withRoles(testRequest(imageNeed("a")), "rpc"), res.Host.Status)
		assert.Equal(t, []string{"rpc"}, res.Host.Roles)

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
	t.Parallel()

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
	t.Parallel()

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

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

// setHold sets the controller-side hold switch of one host, as staff does.
func setHold(ctx context.Context, s Store, name string, wanted bool, reason string) error {
	return s.Commit(ctx, &Changes{Holds: []HostHold{{Host: name, Wanted: wanted, Reason: reason}}})
}

// The broken marks are the host's: a commit replaces them, a sync leaves them
// and they go with the host. A host that does not exist has none.
func TestCommitBrokenMarks(t *testing.T) {
	t.Parallel()

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

// The end of an unproven lease is the host's like its broken marks: a commit
// sets and clears it, a sync leaves it.
func TestCommitUnproven(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newFileStore(t)

	addHost(t, s, "node-1", nil)

	unproven := func() *time.Time {
		host, err := s.GetHost(ctx, "node-1")
		require.NoError(t, err)

		return host.Unproven
	}

	assert.Nil(t, unproven())

	ended := at(5)
	require.NoError(t, s.Commit(ctx, &Changes{Unproven: []HostUnproven{
		{Host: "node-1", At: &ended},
		{Host: "nope", At: &ended},
	}}))
	require.NotNil(t, unproven())
	assert.True(t, ended.Equal(*unproven()))

	_, err := s.RecordSync(ctx, &HostSync{Cert: testCert("node-1"), Request: testRequest(), At: at(10)})
	require.NoError(t, err)
	require.NotNil(t, unproven())

	require.NoError(t, s.Commit(ctx, &Changes{Unproven: []HostUnproven{{Host: "node-1"}}}))
	assert.Nil(t, unproven())
}

// TestHoldBy: a hold remembers that it was set through a team feature only
// when the writer says so. Every writer that does not (a hold without By, a
// Changes.Holds entry without By: the scheduler, the end of a lease) makes it
// a hold of staff, also over a team's hold. A lifted hold remembers nothing.
func TestHoldBy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			s := backend.open(t)
			addHost(t, s, "node-1", nil)

			by := func() (wanted bool, reason, by string) {
				h, err := s.GetHost(ctx, "node-1")
				require.NoError(t, err)

				// The list reads the same column.
				hosts, err := s.ListHosts(ctx)
				require.NoError(t, err)
				require.Len(t, hosts, 1)
				assert.Equal(t, h.HoldBy, hosts[0].HoldBy)

				return h.HoldWanted, h.HoldReason, h.HoldBy
			}

			commit := func(hold HostHold) {
				require.NoError(t, s.Commit(ctx, &Changes{Holds: []HostHold{hold}}))
			}

			_, _, who := by()
			assert.Empty(t, who, "a new host")

			commit(HostHold{Host: "node-1", Wanted: true, Reason: "ours", By: fleet.HoldByTeam})
			wanted, reason, who := by()
			assert.True(t, wanted)
			assert.Equal(t, "ours", reason)
			assert.Equal(t, fleet.HoldByTeam, who)

			// The end of a lease extends the reason through Changes.Holds
			// without By: the hold is staff's from then on.
			commit(HostHold{Host: "node-1", Wanted: true, Reason: "ours; lease ended by operator"})
			_, reason, who = by()
			assert.Equal(t, "ours; lease ended by operator", reason)
			assert.Empty(t, who)

			commit(HostHold{Host: "node-1", Wanted: true, Reason: "ours", By: fleet.HoldByTeam})
			require.NoError(t, setHold(ctx, s, "node-1", true, "staff"))
			_, _, who = by()
			assert.Empty(t, who, "a hold without a writer is not a team's")

			// Lifted: nothing is remembered, whatever the writer says.
			commit(HostHold{Host: "node-1", Wanted: false, By: fleet.HoldByTeam})
			wanted, _, who = by()
			assert.False(t, wanted)
			assert.Empty(t, who)
		})
	}
}

// TestContainerOwnersAreStored: the owners an agent reports come back with
// the host, also after the store was read again from its row, and are gone
// when a later sync reports none.
func TestContainerTeamsAreStored(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			s := backend.open(t)

			req := testRequest()
			req.Containers[0].Kind, req.Containers[0].Team = protocol.KindEL, "geth"

			res, err := s.RecordSync(ctx, &HostSync{
				Cert: testCert("node-1"), Request: req, At: at(0),
			})
			require.NoError(t, err)
			assert.Equal(t, "geth", res.Host.Status.Containers[0].Team)

			host, err := s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			assert.Equal(t, "geth", host.Status.Containers[0].Team)

			hosts, err := s.ListHosts(ctx)
			require.NoError(t, err)
			require.Len(t, hosts, 1)
			assert.Equal(t, "geth", hosts[0].Status.Containers[0].Team)

			// The next sync reports none: the team is gone.
			res, err = s.RecordSync(ctx, &HostSync{
				Cert: testCert("node-1"), Request: testRequest(), At: at(30),
			})
			require.NoError(t, err)
			assert.Empty(t, res.Host.Status.Containers[0].Team)
		})
	}
}
