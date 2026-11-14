package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// nodeWith is a node at head whose clients have the given numbers of peers.
func nodeWith(elPeers, clPeers int) protocol.NodeHealth {
	return protocol.NodeHealth{
		EL: &protocol.ELHealth{Reachable: true, Peers: elPeers},
		CL: &protocol.CLHealth{Reachable: true, Peers: clPeers},
	}
}

// verifying builds a fleet of hosts hosts, each running a node, in which "a"
// (with weight validators) has just reported its operation done.
func verifying(t *testing.T, hosts int, weight uint64) *worldBuilder {
	t.Helper()

	b := newWorldBuilder(t)
	b.host("a", weight)
	b.wants("a", packagesNeed("2.0"))

	for i := 1; i < hosts; i++ {
		b.host("other"+string(rune('0'+i)), 0).Status.Node = nodeWith(9, 9)
	}

	b.step()
	require.Equal(t, []string{"a"}, b.leasedHosts())
	b.finish("a")
	b.step()
	require.Equal(t, fleet.LeaseVerifying, b.lease("a").State)

	return b
}

func TestReadinessNeedsPeers(t *testing.T) {
	// Seven nodes: minPeers 3 is what the node must have.
	b := verifying(t, 7, 0)

	for _, tt := range []struct {
		node protocol.NodeHealth
		note string
	}{
		{nodeWith(0, 9), "waiting for peers: EL has 0 of 3"},
		{nodeWith(9, 1), "waiting for peers: CL has 1 of 3"},
		{nodeWith(2, 2), "waiting for peers: EL has 2 of 3"},
	} {
		b.get("a").Status.Node = tt.node
		b.advance(5 * time.Second)
		b.step()
		require.NotNil(t, b.lease("a"))
		assert.Equal(t, tt.note, b.lease("a").Readiness)
		assert.Nil(t, b.lease("a").SyncedSince)
	}

	b.get("a").Status.Node = nodeWith(3, 3)
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a").SyncedSince)
	assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", b.lease("a").Readiness)

	// Losing a peer starts the time over.
	b.get("a").Status.Node = nodeWith(3, 2)
	b.advance(testEpoch)
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Nil(t, b.lease("a").SyncedSince)

	b.get("a").Status.Node = nodeWith(3, 3)
	b.advance(5 * time.Second)
	b.step()
	b.advance(testEpoch)

	out := b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, "ready: node at head with peers", releasedReadiness(t, out))
}

func TestReadinessMinPeersOnSmallFleets(t *testing.T) {
	for _, tt := range []struct {
		name     string
		hosts    int
		tooling  int // hosts without a node, on top of hosts
		minPeers int
		peers    int
		note     string // empty: ready
	}{
		{name: "a single node still needs a peer", hosts: 1, minPeers: 3, peers: 0, note: "waiting for peers: EL has 0 of 1"},
		{name: "two nodes", hosts: 2, minPeers: 3, peers: 1},
		{name: "three nodes need one peer", hosts: 3, minPeers: 3, peers: 1},
		{name: "three nodes, no peer", hosts: 3, minPeers: 3, peers: 0, note: "waiting for peers: EL has 0 of 1"},
		{name: "four nodes need two", hosts: 4, minPeers: 3, peers: 2},
		{name: "four nodes, one peer", hosts: 4, minPeers: 3, peers: 1, note: "waiting for peers: EL has 1 of 2"},
		{name: "five nodes need two", hosts: 5, minPeers: 3, peers: 2},
		{name: "six nodes need three", hosts: 6, minPeers: 3, peers: 2, note: "waiting for peers: EL has 2 of 3"},
		{name: "seven nodes need three", hosts: 7, minPeers: 3, peers: 3},
		{name: "a large fleet asks for minPeers, not more", hosts: 9, minPeers: 3, peers: 3},
		{name: "minPeers 1", hosts: 9, minPeers: 1, peers: 1},
		{name: "minPeers 5 on nine nodes is half of the others", hosts: 9, minPeers: 5, peers: 4},
		{name: "minPeers 5, three peers", hosts: 9, minPeers: 5, peers: 3, note: "waiting for peers: EL has 3 of 4"},
		// Finding 5: a host without a node is nobody's peer.
		{name: "hosts without a node do not count", hosts: 3, tooling: 4, minPeers: 3, peers: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := verifying(t, tt.hosts, 0)

			for i := range tt.tooling {
				b.host("tooling"+string(rune('0'+i)), 0)
			}

			// A host that has never synced runs no node the controller knows of.
			b.host("unseen", 0).Status = nil

			b.w.Settings.MinPeers = tt.minPeers
			b.w.Settings.ReadyEpochs = 0
			b.get("a").Status.Node = nodeWith(tt.peers, tt.peers)
			b.advance(5 * time.Second)
			b.step()

			if tt.note == "" {
				assert.Nil(t, b.lease("a"), "released")

				return
			}

			require.NotNil(t, b.lease("a"))
			assert.Equal(t, tt.note, b.lease("a").Readiness)
		})
	}
}

// TestReadinessOfAValidatorHostNeedsBoth: the node at head with peers and the
// validators attesting, each for readyEpochs.
func TestReadinessOfAValidatorHostNeedsBoth(t *testing.T) {
	healthy := nodeWith(8, 8)
	syncing := nodeWith(8, 8)
	syncing.CL.Syncing = true

	t.Run("validators attest but the node is syncing", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.get("a").Status.Node = syncing
		b.advance(5 * time.Second)
		b.step()
		b.advance(3 * testEpoch)
		b.step()

		require.NotNil(t, b.lease("a"), "full attestation alone does not release")
		assert.Equal(t, "node is syncing", b.lease("a").Readiness)

		// The node catches up: one epoch at head, then released.
		b.get("a").Status.Node = healthy
		b.advance(5 * time.Second)
		b.step()
		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "node at head with peers, waiting for it to stay so for 6m24s", b.lease("a").Readiness)

		b.advance(testEpoch)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "ready: node at head with peers, validators attesting", releasedReadiness(t, out))
	})

	t.Run("the node is at head with peers but the validators do not attest", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.liveOverride["a"] = 0
		b.get("a").Status.Node = healthy
		b.advance(5 * time.Second)
		b.step()
		b.advance(3 * testEpoch)
		b.step()

		require.NotNil(t, b.lease("a"), "healthy probes alone do not release")
		assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

		// They attest in the next epoch.
		delete(b.liveOverride, "a")
		b.advance(testEpoch)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "ready: node at head with peers, validators attesting", releasedReadiness(t, out))
	})

	t.Run("too few peers although the validators attest", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.get("a").Status.Node = nodeWith(8, 1)
		b.advance(5 * time.Second)
		b.step()
		b.advance(3 * testEpoch)
		b.step()

		require.NotNil(t, b.lease("a"))
		assert.Equal(t, "waiting for peers: CL has 1 of 2", b.lease("a").Readiness)
	})

	t.Run("liveness not served: the node alone decides", func(t *testing.T) {
		b := verifying(t, 5, 10)
		b.withoutLiveness()
		b.get("a").Status.Node = healthy
		b.advance(5 * time.Second)
		b.step()
		b.advance(testEpoch)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "ready: node at head with peers", releasedReadiness(t, out))
	})
}

// releasedReadiness returns the readiness note of the lease.released audit
// entry of an outcome.
func releasedReadiness(t *testing.T, out *Outcome) string {
	t.Helper()

	for _, e := range out.Audit {
		if e.Action == "lease.released" {
			note, ok := e.Detail["readiness"].(string)
			require.True(t, ok)

			return note
		}
	}

	t.Fatal("no lease.released audit entry")

	return ""
}

func TestProbeLivenessNeedsOnePeer(t *testing.T) {
	// The budget's probe rule asks for a peer, not for minPeers: a node with
	// one peer attests.
	b := newWorldBuilder(t)
	b.withoutLiveness()

	for _, name := range []string{"one", "none", "c", "d", "e"} {
		b.host(name, 10).Status.Node = nodeWith(9, 9)
	}

	b.get("one").Status.Node = nodeWith(1, 1)
	b.get("none").Status.Node = nodeWith(1, 0)

	out := b.step()
	assert.Equal(t, uint64(10), out.Plan.Hosts["one"].Live)
	assert.Equal(t, uint64(0), out.Plan.Hosts["none"].Live)
	assert.Equal(t, uint64(10), out.Plan.Budget.UnavailableWeight)
}

// Finding 4: a host with validators is not released on its probes alone while
// the beacon node has not said yet whether it serves liveness. The beacon
// service publishes the epoch timing before it publishes liveness, so every
// controller restart passes through this state, with the run of healthy syncs
// already stored on the lease.
func TestReview1Finding4ValidatorHostIsNotReadyWhileLivenessIsUnknown(t *testing.T) {
	b := verifying(t, 5, 10)
	b.get("a").Status.Node = nodeWith(8, 8)
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)

	// The controller restarts: timing is known, liveness is not yet.
	b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
	b.refreshHealth()
	b.liveOverride["a"] = 0

	out := b.step()
	require.NotNil(t, b.lease("a"), "not released without the attestation check")
	assert.Equal(t, fleet.LeaseVerifying, b.lease("a").State)
	assert.Equal(t, "waiting for attestation data from the beacon node", b.lease("a").Readiness)
	assert.NotNil(t, b.lease("a").SyncedSince, "the run of healthy syncs is kept")
	assert.NotContains(t, auditActions(out), "lease.released")

	// Liveness arrives and says the validators do not attest.
	b.healthMod = nil
	b.advance(5 * time.Second)
	b.step()
	require.NotNil(t, b.lease("a"))
	assert.Equal(t, "validators attesting: 0 of 1 epochs with at least 9 of 10", b.lease("a").Readiness)

	// They attest for a complete epoch: released.
	delete(b.liveOverride, "a")
	b.advance(testEpoch)

	out = b.step()
	require.Nil(t, b.lease("a"))
	assert.Equal(t, "ready: node at head with peers, validators attesting", releasedReadiness(t, out))

	// A host without validators does not wait for the beacon node.
	t.Run("host without validators", func(t *testing.T) {
		b := verifying(t, 5, 0)
		b.healthMod = func(n *fleet.NetworkHealth) { n.LivenessKnown = false }
		b.refreshHealth()
		b.get("a").Status.Node = nodeWith(8, 8)
		b.advance(5 * time.Second)
		b.step()
		b.advance(testEpoch)

		out := b.step()
		require.Nil(t, b.lease("a"))
		assert.Equal(t, "ready: node at head with peers", releasedReadiness(t, out))
	})
}
