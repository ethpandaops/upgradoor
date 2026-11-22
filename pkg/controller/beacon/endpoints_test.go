package beacon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// states returns the state of every endpoint.
func (h *harness) states() []EndpointState {
	got := h.svc.Endpoints()
	out := make([]EndpointState, 0, len(got))

	for i := range got {
		out = append(out, got[i].State)
	}

	return out
}

// blockRequests sums the requests for one block over all nodes.
func (h *harness) blockRequests(root string) int {
	total := 0
	for _, node := range h.nodes {
		total += node.count(blockPrefix + root)
	}

	return total
}

func TestEndpointsAreAskedInParallel(t *testing.T) {
	h := newHarness(t, 3, slotHosts(), nil)
	newSlotChain(t, h, 70, 82)

	require.NoError(t, h.svc.loadTiming(t.Context()))

	// Every node holds its answers until all of them were asked.
	release := make(chan struct{})

	for _, node := range h.nodes {
		node.set(func(n *fakeNode) { n.block = release })
	}

	h.clock.Set(testGenesis.Add(82*testSlot + testSlot/2))

	done := make(chan struct{})

	go func() {
		defer close(done)
		h.svc.poll(t.Context())
	}()

	require.Eventually(t, func() bool {
		for _, node := range h.nodes {
			if node.count(pathHead) == 0 {
				return false
			}
		}

		return true
	}, 10*time.Second, time.Millisecond)

	for _, node := range h.nodes {
		node.set(func(n *fakeNode) { n.block = nil })
	}

	close(release)
	<-done

	assert.Empty(t, h.svc.Health().Error)
	assert.Equal(t, []EndpointState{EndpointOK, EndpointOK, EndpointOK}, h.states())

	for _, node := range h.nodes {
		assert.Equal(t, 1, node.count(pathSyncing))
		assert.Equal(t, 1, node.count(pathFinality))
	}

	// All agree: every block is read once, from one endpoint.
	for slot := uint64(71); slot <= 82; slot++ {
		assert.Equal(t, 1, h.blockRequests(slotRoot(slot, "")), "block of slot %d", slot)
	}
}

func TestEndpointStates(t *testing.T) {
	h := newHarness(t, 5, slotHosts(), nil)
	chain := newSlotChain(t, h, 70, 84)

	setHead(h.nodes[2], slotRoot(81, ""), 81)
	h.nodes[1].set(func(n *fakeNode) { n.finalized = 9 })
	h.nodes[2].set(func(n *fakeNode) { n.finalized = 10 })
	h.nodes[3].set(func(n *fakeNode) { n.elOffline = true })
	h.nodes[4].set(func(n *fakeNode) { n.down = true })

	chain.pollAt(t, 85)

	assert.Equal(t, []EndpointState{
		EndpointOK, EndpointOK, EndpointBehind, EndpointSyncing, EndpointDown,
	}, h.states())

	health := h.svc.Health()
	assert.Empty(t, health.Error)
	assert.Empty(t, health.Split)
	assert.Equal(t, uint64(84), health.HeadSlot)
	assert.Equal(t, uint64(9), health.FinalizedEpoch, "the highest of the ok endpoints")

	row, ok := h.store.slot(82)
	require.True(t, ok)
	assert.Equal(t, full(82), row)

	for slot := uint64(71); slot <= 84; slot++ {
		assert.Equal(t, 1, h.blockRequests(slotRoot(slot, "")), "block of slot %d", slot)
	}

	// Two slots below the head is not behind yet; a node that says it is
	// syncing is.
	setHead(h.nodes[2], slotRoot(82, ""), 82)
	h.nodes[3].set(func(n *fakeNode) {
		n.elOffline = false
		n.syncing = true
	})
	chain.pollAt(t, 85)

	assert.Equal(t, []EndpointState{
		EndpointOK, EndpointOK, EndpointOK, EndpointSyncing, EndpointDown,
	}, h.states())
	assert.Equal(t, uint64(10), h.svc.Health().FinalizedEpoch)
}

func TestEndpointsNoneInSync(t *testing.T) {
	h := newHarness(t, 1, newFakeStore(), nil)
	h.svc.poll(t.Context())
	require.Empty(t, h.svc.Health().Error)

	h.node.set(func(n *fakeNode) { n.syncing = true })
	h.clock.Advance(testSlot)
	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.Contains(t, got.Error, "no beacon endpoint is usable")
	assert.Contains(t, got.Error, "syncing")
	assert.Equal(t, at(10, 30*time.Second), got.UpdatedAt, "the last view stays")
	assert.Equal(t, []EndpointState{EndpointSyncing}, h.states())
}

func TestEndpointsMinorityFork(t *testing.T) {
	h := newHarness(t, 3, slotHosts(), nil)
	chain := newSlotChain(t, h, 70, 82)
	chain.pollAt(t, 82)

	// One node follows its own block of slot 83 on top of slot 82.
	chain.add(83, "", vote(82, true, true))
	chain.add(84, "", vote(83, true, true))
	fork := chain.serve(slotRoot(82, ""), 83, "ff")
	setHead(h.nodes[2], fork, 83)

	chain.pollAt(t, 85)

	assert.Equal(t, []EndpointState{EndpointOK, EndpointOK, EndpointForked}, h.states())

	health := h.svc.Health()
	assert.Empty(t, health.Error)
	assert.Empty(t, health.Split, "a minority fork is no split")
	assert.Equal(t, uint64(84), health.HeadSlot)

	// The fork is read once, however long it stands.
	chain.pollAt(t, 85)

	assert.Equal(t, []EndpointState{EndpointOK, EndpointOK, EndpointForked}, h.states())
	assert.Equal(t, 1, h.blockRequests(fork))
	assert.Equal(t, 1, h.blockRequests(slotRoot(84, "")))
}

func TestEndpointsSplit(t *testing.T) {
	h := newHarness(t, 5, slotHosts(), nil)
	chain := newSlotChain(t, h, 70, 82)
	chain.pollAt(t, 82)

	// Two chains on top of slot 82, held by two endpoints each. The fifth is
	// still at slot 82, which is on both: each has a majority.
	chain.add(83, "", vote(82, true, true))

	fork := chain.serve(slotRoot(82, ""), 83, "ff")
	setHead(h.nodes[2], fork, 83)
	setHead(h.nodes[3], fork, 83)
	setHead(h.nodes[4], slotRoot(82, ""), 82)

	chain.pollAt(t, 84)

	assert.Empty(t, h.svc.Health().Split)
	assert.Equal(t, slotRoot(83, ""), h.svc.head.root, "the tie keeps the chain followed")
	assert.Equal(t, []EndpointState{
		EndpointOK, EndpointOK, EndpointForked, EndpointForked, EndpointOK,
	}, h.states())

	// Without the fifth, neither chain has a majority.
	h.nodes[4].set(func(n *fakeNode) { n.down = true })
	chain.pollAt(t, 84)

	health := h.svc.Health()
	assert.Empty(t, health.Error)
	assert.Equal(t, "2 chains, 2/4 and 2/4 endpoints", health.Split)
	assert.Equal(t, slotRoot(83, ""), h.svc.head.root)

	// It comes back on a third chain.
	fork2 := chain.serve(slotRoot(82, ""), 83, "ee")
	setHead(h.nodes[4], fork2, 83)
	h.nodes[4].set(func(n *fakeNode) { n.down = false })

	chain.pollAt(t, 84)

	assert.Equal(t, "3 chains, 2/5, 2/5 and 1/5 endpoints", h.svc.Health().Split)

	// The fork gains a third endpoint: it is followed, and that is no split.
	setHead(h.nodes[4], fork, 83)
	chain.pollAt(t, 84)

	health = h.svc.Health()
	assert.Empty(t, health.Split)
	assert.Equal(t, fork, h.svc.head.root)
	assert.Equal(t, []EndpointState{
		EndpointForked, EndpointForked, EndpointOK, EndpointOK, EndpointOK,
	}, h.states())

	for _, root := range []string{fork, fork2, slotRoot(83, "")} {
		assert.Equal(t, 1, h.blockRequests(root), "block %s", root)
	}
}

func TestEndpointsBlocksFromOKEndpointsOnly(t *testing.T) {
	h := newHarness(t, 4, slotHosts(), nil)
	chain := newSlotChain(t, h, 70, 82)

	syncing, down, failing, good := h.nodes[0], h.nodes[1], h.nodes[2], h.nodes[3]

	syncing.set(func(n *fakeNode) { n.syncing = true })
	down.set(func(n *fakeNode) { n.down = true })
	failing.set(func(n *fakeNode) { n.failPath = blockPrefix + slotRoot(82, "") })

	chain.pollAt(t, 82)

	assert.Empty(t, h.svc.Health().Error)
	assert.Empty(t, h.svc.Health().LivenessError)

	assert.Zero(t, syncing.count(blockPrefix+slotRoot(82, "")))
	assert.Equal(t, 1, failing.count(blockPrefix+slotRoot(82, "")))
	assert.Equal(t, 1, good.count(blockPrefix+slotRoot(82, "")), "the next ok endpoint")
	assert.Equal(t, 1, failing.count(blockPrefix+slotRoot(81, "")), "the first ok endpoint again")
	assert.Zero(t, good.count(blockPrefix+slotRoot(81, "")))

	for _, node := range []*fakeNode{syncing, down} {
		for slot := uint64(71); slot <= 82; slot++ {
			assert.Zero(t, node.count(blockPrefix+slotRoot(slot, "")))
		}

		assert.Zero(t, node.count("/eth/v1/beacon/states/head/committees?epoch=9"))
	}
}
