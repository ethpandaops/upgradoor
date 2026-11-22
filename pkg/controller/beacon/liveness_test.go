package beacon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// The liveness tests run on the slot test chain (slots_test.go): host a has
// one duty per slot, host b too, and host idle has validators without any.
func livenessHosts() *fakeStore {
	return newFakeStore(host("a", 0, 8), host("b", 8, 16), host("idle", 500, 504))
}

func TestLivenessIsSummedFromTheSlots(t *testing.T) {
	h := newHarness(t, 1, livenessHosts(), nil)
	chain := newSlotChain(t, h, 70, 73)

	// Slot 74: host b's attestation does not make it into a block yet.
	chain.add(74, "", vote(73, true, true))
	chain.add(75, "", vote(74, true, false))

	for slot := uint64(76); slot <= 82; slot++ {
		chain.add(slot, "", vote(slot-1, true, true))
	}

	chain.pollAt(t, 82)

	assert.Equal(t, []fleet.Liveness{
		{Host: "a", Epoch: 9, Live: 8, Total: 8},
		{Host: "b", Epoch: 9, Live: 7, Total: 8},
		{Host: "idle", Epoch: 9},
	}, h.svc.Liveness(4), "epoch 10 is not settled; a host without duties has nothing live")

	got := h.svc.Health()
	assert.True(t, got.LivenessKnown)
	assert.Equal(t, uint64(9), got.LivenessEpoch)
	assert.Equal(t, uint64(16), got.Tracked)
	assert.Equal(t, uint64(15), got.Live)

	// Settle rule: epoch 10 (slots 80..87) counts once the clock is two slots
	// past its last slot, in slot 89.
	for slot := uint64(83); slot <= 89; slot++ {
		votes := []map[string]any{vote(slot-1, true, true)}
		if slot == 83 {
			// Host b's vote of slot 74 is included late, within epoch 10.
			votes = append(votes, vote(74, false, true))
		}

		chain.add(slot, "", votes...)
	}

	chain.pollAt(t, 88)
	assert.Equal(t, uint64(9), h.svc.Health().LivenessEpoch)
	assert.Equal(t, uint64(16), h.svc.Health().Live, "the late vote revises epoch 9")

	chain.pollAt(t, 89)
	assert.Equal(t, uint64(10), h.svc.Health().LivenessEpoch)

	rows := h.svc.Liveness(2)
	require.Len(t, rows, 6)
	assert.Equal(t, fleet.Liveness{Host: "a", Epoch: 10, Live: 8, Total: 8}, rows[0], "newest first")
	assert.Equal(t, fleet.Liveness{Host: "b", Epoch: 9, Live: 8, Total: 8}, rows[4])
}

func TestLivenessFailsClosed(t *testing.T) {
	h := newHarness(t, 1, livenessHosts(), nil)
	chain := newSlotChain(t, h, 70, 82)

	// Nobody can say who had to attest in epoch 9: it gets no liveness.
	h.node.set(func(n *fakeNode) { n.committeesStatus = map[uint64]int{9: 400} })
	chain.pollAt(t, 82)

	got := h.svc.Health()
	assert.False(t, got.LivenessKnown)
	assert.Empty(t, h.svc.Liveness(4))

	for slot := uint64(83); slot <= 89; slot++ {
		chain.add(slot, "", vote(slot-1, true, true))
	}

	chain.pollAt(t, 89)

	got = h.svc.Health()
	assert.True(t, got.LivenessKnown)
	assert.Equal(t, uint64(10), got.LivenessEpoch)
	assert.Len(t, h.svc.Liveness(4), 3, "epoch 9 stays missing")

	// A count that fails publishes nothing new and says why.
	for slot := uint64(90); slot <= 97; slot++ {
		chain.add(slot, "", vote(slot-1, true, true))
	}

	h.node.set(func(n *fakeNode) { n.failPath = blockPrefix + slotRoot(90, "") })
	chain.pollAt(t, 97)

	got = h.svc.Health()
	assert.Equal(t, uint64(10), got.LivenessEpoch)
	assert.Contains(t, got.LivenessError, "reading block")
}

func TestLivenessKeepsABoundedHistory(t *testing.T) {
	tracker := newSlotTracker()
	svc := &service{slots: tracker}

	for epoch := range uint64(livenessEpochs + 5) {
		svc.publishLiveness([]epochLiveness{{epoch: epoch, rows: []fleet.Liveness{
			{Host: "a", Epoch: epoch, Live: 1, Total: 1},
		}}}, "")
	}

	assert.Len(t, svc.liveness, livenessEpochs)
	assert.Equal(t, uint64(5), svc.liveness[0].epoch)
	assert.Equal(t, uint64(livenessEpochs+4), svc.Health().LivenessEpoch)
	assert.Len(t, svc.Liveness(1000), livenessEpochs)
}

// span returns the slots [from, to].
func span(from, to uint64) []uint64 {
	out := make([]uint64, 0, to-from+1)
	for slot := from; slot <= to; slot++ {
		out = append(out, slot)
	}

	return out
}

func TestAttestedSlotsFollowTheChain(t *testing.T) {
	h := newHarness(t, 1, livenessHosts(), nil)
	chain := newSlotChain(t, h, 70, 82)

	assert.Nil(t, h.svc.AttestedSlots(), "nothing before the first poll")

	chain.add(83, "", vote(82, true, true))
	chain.pollAt(t, 84)

	// From the start of the previous epoch (slot 72) to the newest settled
	// slot; host idle has no duties.
	before := h.svc.AttestedSlots()
	assert.Equal(t, map[string][]uint64{"a": span(72, 82), "b": span(72, 82)}, before)

	// The blocks of slots 82 and 83 lose against a branch on top of slot 81
	// whose block of slot 84 carries host a's attestation of slot 81 only.
	chain.addOn(slotRoot(81, ""), 84, "ff", vote(81, true, false))
	chain.add(85, "ff", vote(84, true, true))
	chain.pollAt(t, 85)

	assert.Equal(t, map[string][]uint64{"a": span(72, 81), "b": span(72, 80)}, h.svc.AttestedSlots(),
		"what only the orphaned blocks included is gone")
	assert.Equal(t, span(72, 82), before["a"], "a published map is not changed afterwards")

	// Only the newest slots are kept, also of slots that left the window.
	for slot := uint64(86); slot <= 130; slot++ {
		chain.add(slot, "ff", vote(slot-1, true, slot%2 == 0))
		chain.pollAt(t, slot)
	}

	got := h.svc.AttestedSlots()
	assert.Equal(t, span(128-fleet.MaxReadyAttestations+1, 128), got["a"])
	require.Len(t, got["b"], fleet.MaxReadyAttestations)
	assert.Equal(t, []uint64{123, 125, 127}, got["b"][len(got["b"])-3:], "b attests in odd slots only")
}

func TestAttestedSlotsOfAHostThatLeaves(t *testing.T) {
	tracker := newSlotTracker()
	duty := func(host string, included uint64) fleet.HostSlotParticipation {
		return fleet.HostSlotParticipation{Host: host, Assigned: 1, Included: included}
	}

	tracker.noteAttested(5, nil, []fleet.HostSlotParticipation{duty("a", 1), duty("b", 0)})
	tracker.noteAttested(3, nil, []fleet.HostSlotParticipation{duty("a", 1), duty("b", 1)})
	assert.Equal(t, map[string][]uint64{"a": {3, 5}, "b": {3}}, tracker.attested)

	// A recount of slot 3 that finds b's attestation no longer included.
	tracker.noteAttested(3, []fleet.HostSlotParticipation{duty("a", 1), duty("b", 1)},
		[]fleet.HostSlotParticipation{duty("a", 1), duty("b", 0)})
	assert.Equal(t, map[string][]uint64{"a": {3, 5}}, tracker.attested)

	tracker.setHosts([]trackedHost{{name: "b", start: 0, end: 1}}, 1)
	assert.Empty(t, tracker.attested, "a is no longer tracked")
	assert.True(t, tracker.attestedChanged)
}
