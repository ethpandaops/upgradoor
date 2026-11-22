package beacon

import (
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// bitfield packs bits the SSZ way: bit i is bit i%8 of byte i/8.
func bitfield(bytes int, set ...int) []byte {
	out := make([]byte, bytes)
	for _, index := range set {
		out[index/8] |= 1 << (index % 8)
	}

	return out
}

// bitlist is a bitfield of length bits with its length bit.
func bitlist(length int, set ...int) []byte {
	return bitfield(length/8+1, append(set, length)...)
}

func TestBitlistLength(t *testing.T) {
	tests := []struct {
		name   string
		list   []byte
		length int
		ok     bool
	}{
		{name: "empty list", list: []byte{0x01}, length: 0, ok: true},
		{name: "eight bits, all set (lighthouse 0xff01)", list: []byte{0xff, 0x01}, length: 8, ok: true},
		{name: "three bits", list: []byte{0b1101}, length: 3, ok: true},
		{name: "length bit in the top position", list: []byte{0x00, 0x80}, length: 15, ok: true},
		{name: "no bytes", list: nil},
		{name: "no length bit", list: []byte{0xff, 0x00}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			length, ok := bitlistLength(tt.list)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.length, length)
		})
	}
}

func TestAttestationAttesters(t *testing.T) {
	// Three committees of different sizes in one slot.
	committees := [][]uint64{
		{10, 11, 12},
		{20, 21, 22, 23, 24, 25, 26, 27, 28},
		{30, 31},
	}

	tests := []struct {
		name string
		vote attestation
		want []uint64
		err  string
	}{
		{
			name: "before Electra: data.index names the committee",
			vote: attestation{index: 1, aggregationBits: bitlist(9, 0, 8)},
			want: []uint64{20, 28},
		},
		{
			name: "before Electra: nobody signed",
			vote: attestation{index: 0, aggregationBits: bitlist(3)},
			want: []uint64{},
		},
		{
			name: "Electra: one committee by its bit, data.index is 0",
			vote: attestation{committeeBits: bitfield(8, 2), aggregationBits: bitlist(2, 1)},
			want: []uint64{31},
		},
		{
			name: "Electra: committees 0 and 2, bits one after the other",
			// Positions 0..2 are committee 0, positions 3..4 committee 2.
			vote: attestation{committeeBits: bitfield(8, 0, 2), aggregationBits: bitlist(5, 1, 3, 4)},
			want: []uint64{11, 30, 31},
		},
		{
			name: "Electra: all three, the second one crosses a byte",
			// 0..2 committee 0, 3..11 committee 1, 12..13 committee 2.
			vote: attestation{
				committeeBits:   bitfield(8, 0, 1, 2),
				aggregationBits: bitlist(14, 0, 3, 7, 8, 11, 13),
			},
			want: []uint64{10, 20, 24, 25, 28, 31},
		},
		{
			name: "Electra: the index is ignored when committee bits are there",
			vote: attestation{index: 1, committeeBits: bitfield(8, 0), aggregationBits: bitlist(3, 2)},
			want: []uint64{12},
		},
		{
			name: "bits for another committee size",
			vote: attestation{index: 0, aggregationBits: bitlist(9, 0)},
			err:  "cover 9 validators, the committees have 3",
		},
		{
			name: "Electra: bits that leave out a selected committee",
			vote: attestation{committeeBits: bitfield(8, 0, 2), aggregationBits: bitlist(3, 0)},
			err:  "cover 3 validators, the committees have 5",
		},
		{
			name: "a committee the slot does not have",
			vote: attestation{index: 3, aggregationBits: bitlist(2, 0)},
			err:  "committee 3 of 3",
		},
		{
			name: "Electra: a committee bit the slot does not have",
			vote: attestation{committeeBits: bitfield(8, 1, 5), aggregationBits: bitlist(9, 0)},
			err:  "committee 5 of 3",
		},
		{
			name: "Electra: no committee selected",
			vote: attestation{committeeBits: bitfield(8), aggregationBits: bitlist(0)},
			err:  "no committee selected",
		},
		{
			name: "no length bit",
			vote: attestation{index: 0, aggregationBits: []byte{0x00}},
			err:  "no length bit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make([]uint64, 0, 8)

			err := tt.vote.attesters(committees, func(validator uint64) { got = append(got, validator) })
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				assert.Empty(t, got, "an aggregate that does not fit marks nobody")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSlotTrackerParticipation(t *testing.T) {
	// Slot 40: two committees. Host a has 0..3, host b 4..7, and host twin
	// shares validator 3 with a.
	committees := [][]uint64{{0, 100, 4, 3}, {101, 5, 1}}
	hosts := []trackedHost{
		{name: "a", start: 0, end: 4},
		{name: "twin", start: 3, end: 4},
		{name: "b", start: 4, end: 8},
	}

	electra := func(slot uint64, bits []byte, selected ...int) attestation {
		return attestation{slot: slot, committeeBits: bitfield(8, selected...), aggregationBits: bits}
	}

	tracker := newSlotTracker()
	tracker.extend([]*chainBlock{
		// The block of the slot itself cannot carry its attestations.
		{slot: 40, root: "0x40", attestations: []attestation{electra(40, bitlist(4, 0, 1, 2, 3), 0)}},
		{slot: 41, root: "0x41", attestations: []attestation{
			// Validator 0 and the outsider 100.
			electra(40, bitlist(4, 0, 1), 0),
			// Validator 0 again in a second aggregate, with validator 4.
			electra(40, bitlist(4, 0, 2), 0),
			// Another slot's aggregate is not this slot's.
			electra(39, bitlist(4, 3), 0),
		}},
		// Slot 42 has no block. Validator 5 gets in late, in an aggregate
		// over both committees that names validator 0 a third time.
		{slot: 43, root: "0x43", attestations: []attestation{electra(40, bitlist(7, 0, 5), 0, 1)}},
	})

	row, duties, err := tracker.participation(40, committees, hosts)
	require.NoError(t, err)
	assert.Equal(t, []fleet.HostSlotParticipation{
		{Host: "a", Assigned: 3, Included: 1},
		{Host: "twin", Assigned: 1, Included: 0},
		{Host: "b", Assigned: 2, Included: 2},
	}, duties, "the duties name every host with one, for the per-epoch liveness")
	assert.Equal(t, fleet.SlotParticipation{
		Slot: 40, Proposed: true,
		// 0, 1, 3, 4, 5 have a duty; 0, 4 and 5 are included, each once.
		Assigned: 5, Included: 3,
		Hosts: []fleet.HostSlotParticipation{
			{Host: "a", Assigned: 3, Included: 1},
			{Host: "twin", Assigned: 1, Included: 0},
		},
	}, row, "a validator counts once however often it is included, and once fleet-wide in two ranges; "+
		"b attested everything and is not listed")

	missed, _, err := tracker.participation(42, [][]uint64{{6, 2}}, hosts)
	require.NoError(t, err)
	assert.Equal(t, fleet.SlotParticipation{
		Slot: 42, Assigned: 2,
		Hosts: []fleet.HostSlotParticipation{
			{Host: "a", Assigned: 1}, {Host: "b", Assigned: 1},
		},
	}, missed, "a slot without a block, and nothing included of it")

	// A host with two ranges is one host.
	split := []trackedHost{
		{name: "a", start: 0, end: 2}, {name: "b", start: 2, end: 4}, {name: "a", start: 4, end: 8},
	}
	_, duties, err = tracker.participation(42, [][]uint64{{6, 2, 1}}, split)
	require.NoError(t, err)
	assert.Equal(t, []fleet.HostSlotParticipation{
		{Host: "a", Assigned: 2}, {Host: "b", Assigned: 1},
	}, duties)

	// An aggregate that does not fit is reported and left out; the rest of
	// the slot is counted.
	tracker.extend([]*chainBlock{
		{slot: 44, root: "0x44", attestations: []attestation{electra(40, bitlist(9, 0, 1, 2), 0)}},
	})

	row, _, err = tracker.participation(40, committees, hosts)
	require.ErrorContains(t, err, "attestation for slot 40 in the block of slot 44")
	assert.Equal(t, uint64(3), row.Included)

	// Without hosts only the proposal is known.
	bare, _, err := tracker.participation(40, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, fleet.SlotParticipation{Slot: 40, Proposed: true}, bare)
}

// The slot tests run on the test chain of the harness (8 slots per epoch,
// the clock 30 s into epoch 10, which is slot 82) with two hosts: a has the
// validators 0..7, b has 8..15. Every slot has two committees:
//
//	committee 0: validator i of a, an outsider
//	committee 1: two outsiders, validator 8+i of b
//
// where i is the slot's position in its epoch.
const (
	committee0Size = 2
	committee1Size = 3
)

func slotHosts() *fakeStore {
	return newFakeStore(host("a", 0, 8), host("b", 8, 16))
}

func slotCommittees(slot uint64) [][]uint64 {
	position := slot % testSlotsPerEpoch

	return [][]uint64{
		{position, 1000 + slot},
		{2000 + slot, 3000 + slot, 8 + position},
	}
}

// vote is an Electra aggregate for slot as the beacon API returns it. a and
// b say whether the validator of that host signed; the outsiders always do.
func vote(slot uint64, a, b bool) map[string]any {
	set := []int{1, committee0Size, committee0Size + 1}
	if a {
		set = append(set, 0)
	}

	if b {
		set = append(set, committee0Size+2)
	}

	return map[string]any{
		"aggregation_bits": "0x" + hex.EncodeToString(bitlist(committee0Size+committee1Size, set...)),
		"committee_bits":   "0x" + hex.EncodeToString(bitfield(8, 0, 1)),
		"data":             map[string]any{"slot": fmt.Sprint(slot), "index": "0"},
		"signature":        "0x00",
	}
}

func slotRoot(slot uint64, fork string) string {
	return fmt.Sprintf("0x%04x%s", slot, fork)
}

// slotChain grows a chain on the harness's node.
type slotChain struct {
	h *harness
	// tip is the head: its root and slot.
	tip     string
	tipSlot uint64
}

// newSlotChain starts a chain with a block at every slot of [from, to], each
// carrying the full attestations of the slot before it.
func newSlotChain(t *testing.T, h *harness, from, to uint64) *slotChain {
	t.Helper()

	chain := &slotChain{h: h, tip: "0x00"}

	h.node.set(func(n *fakeNode) {
		n.blocks = make(map[string]fakeBlock, 32)
		n.committees = make(map[uint64][][]uint64, 64)

		for slot := uint64(0); slot < 20*testSlotsPerEpoch; slot++ {
			n.committees[slot] = slotCommittees(slot)
		}
	})

	for slot := from; slot <= to; slot++ {
		chain.add(slot, "", vote(slot-1, true, true))
	}

	return chain
}

// add puts a block on the head and makes it the head.
func (c *slotChain) add(slot uint64, fork string, votes ...map[string]any) {
	c.addOn(c.tip, slot, fork, votes...)
}

// addOn puts a block on parent and makes it the head.
func (c *slotChain) addOn(parent string, slot uint64, fork string, votes ...map[string]any) {
	root := slotRoot(slot, fork)

	c.h.node.set(func(n *fakeNode) {
		n.blocks[root] = fakeBlock{slot: slot, parent: parent, attestations: votes}
		n.headRoot = root
		n.headSlot = slot
	})

	c.tip, c.tipSlot = root, slot
}

// pollAt sets the clock to the middle of slot and polls.
func (c *slotChain) pollAt(t *testing.T, slot uint64) {
	t.Helper()

	c.h.clock.Set(testGenesis.Add(time.Duration(slot)*testSlot + testSlot/2))
	c.h.svc.poll(t.Context())
}

func full(slot uint64) fleet.SlotParticipation {
	return fleet.SlotParticipation{
		Slot: slot, Proposed: true, Assigned: 2, Included: 2,
	}
}

func TestSlotsFollowTheChain(t *testing.T) {
	h := newHarness(t, 1, slotHosts(), nil)
	chain := newSlotChain(t, h, 70, 82)

	chain.pollAt(t, 82)

	// The first poll reads back to the start of the previous epoch (slot
	// 72) and the block below it, where it stops; a slot is stored once the
	// clock is two slots past it.
	for slot := uint64(72); slot <= 80; slot++ {
		row, ok := h.store.slot(slot)
		require.True(t, ok, "slot %d", slot)
		assert.Equal(t, full(slot), row)
	}

	for _, slot := range []uint64{71, 81, 82} {
		_, ok := h.store.slot(slot)
		assert.False(t, ok, "slot %d is before the window or not settled", slot)
	}

	assert.Equal(t, 1, h.node.count(blockPrefix+slotRoot(71, "")))
	assert.Zero(t, h.node.count(blockPrefix+slotRoot(70, "")))

	// One new block is one request; committees are read once per epoch.
	chain.add(83, "", vote(82, true, true))
	chain.pollAt(t, 83)
	chain.pollAt(t, 83)

	row, ok := h.store.slot(81)
	require.True(t, ok)
	assert.Equal(t, full(81), row)

	for slot := uint64(71); slot <= 83; slot++ {
		assert.Equal(t, 1, h.node.count(blockPrefix+slotRoot(slot, "")), "block of slot %d", slot)
	}

	committeesPath := "/eth/v1/beacon/states/head/committees?epoch="
	assert.Equal(t, 1, h.node.count(committeesPath+"9"))
	assert.Equal(t, 1, h.node.count(committeesPath+"10"))
	assert.Zero(t, h.node.count("/eth/v1/node/syncing"), "not asked while blocks arrive")

	h.store.set(func(s *fakeStore) {
		assert.Equal(t, 1, s.slotSaves[75], "a slot that did not change is not stored again")
	})
}

func TestSlotsMissedBlockAndLateInclusion(t *testing.T) {
	h := newHarness(t, 1, slotHosts(), nil)
	chain := newSlotChain(t, h, 70, 73)

	// Slot 74: host b's attestation does not make it into the next block.
	// Slot 76 has no block: the block of slot 77 carries the attestations of
	// slots 75 and 76.
	chain.add(74, "", vote(73, true, true))
	chain.add(75, "", vote(74, true, false))
	chain.add(77, "", vote(75, true, true), vote(76, true, true))

	for slot := uint64(78); slot <= 82; slot++ {
		chain.add(slot, "", vote(slot-1, true, true))
	}

	chain.pollAt(t, 82)

	row, _ := h.store.slot(74)
	assert.Equal(t, fleet.SlotParticipation{
		Slot: 74, Proposed: true, Assigned: 2, Included: 1,
		Hosts: []fleet.HostSlotParticipation{
			{Host: "b", Assigned: 1, Included: 0},
		},
	}, row)

	missed := full(76)
	missed.Proposed = false

	row, _ = h.store.slot(76)
	assert.Equal(t, missed, row, "no block in the slot; its attestations are in the next one")

	row, _ = h.store.slot(75)
	assert.Equal(t, full(75), row, "included two slots later, after the missed one")

	// An epoch later (slot 74 is in epoch 9, the block in epoch 10) host b's
	// attestation is included after all: the row is rewritten.
	chain.add(83, "", vote(82, true, true))
	chain.add(84, "", vote(83, true, true), vote(74, false, true))
	chain.pollAt(t, 84)

	row, _ = h.store.slot(74)
	assert.Equal(t, full(74), row)

	h.store.set(func(s *fakeStore) {
		assert.Equal(t, 2, s.slotSaves[74])
		assert.Equal(t, 1, s.slotSaves[75])
	})
}

func TestSlotsReorg(t *testing.T) {
	h := newHarness(t, 1, slotHosts(), nil)
	chain := newSlotChain(t, h, 70, 82)

	chain.add(83, "", vote(82, true, true))
	chain.pollAt(t, 84)

	row, _ := h.store.slot(82)
	assert.Equal(t, full(82), row)

	// The blocks of slots 82 and 83 lose against another branch on top of
	// slot 81: slot 82 has no block there, and its block of slot 84 carries
	// host a's attestation for slot 81 only.
	chain.addOn(slotRoot(81, ""), 84, "ff", vote(81, true, false))
	chain.add(85, "ff", vote(84, true, true))
	chain.pollAt(t, 85)

	row, _ = h.store.slot(81)
	assert.Equal(t, fleet.SlotParticipation{
		Slot: 81, Proposed: true, Assigned: 2, Included: 1,
		Hosts: []fleet.HostSlotParticipation{
			{Host: "b", Assigned: 1, Included: 0},
		},
	}, row, "what the orphaned block of slot 82 included no longer counts")

	for _, slot := range []uint64{82, 83} {
		row, _ = h.store.slot(slot)
		assert.Equal(t, fleet.SlotParticipation{
			Slot: slot, Assigned: 2,
			Hosts: []fleet.HostSlotParticipation{
				{Host: "a", Assigned: 1}, {Host: "b", Assigned: 1},
			},
		}, row, "slot %d has no block on the new branch and nothing of it was included", slot)
	}

	assert.Equal(t, 1, h.node.count(blockPrefix+slotRoot(84, "ff")))
	assert.Equal(t, 1, h.node.count(blockPrefix+slotRoot(81, "")), "the walk stops at the fork")
}

func TestSlotsWithoutNewBlocks(t *testing.T) {
	tests := []struct {
		name    string
		syncing bool
		// newest is the newest slot with a row when the clock is in slot 86.
		newest uint64
	}{
		{name: "the chain stands still", syncing: false, newest: 84},
		{name: "the node is behind", syncing: true, newest: 81},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, 1, slotHosts(), nil)
			chain := newSlotChain(t, h, 70, 82)

			h.node.set(func(n *fakeNode) { n.syncing = tt.syncing })
			chain.pollAt(t, 86)

			row, _ := h.store.slot(81)
			assert.Equal(t, full(81), row)

			for slot := uint64(82); slot <= 86; slot++ {
				row, ok := h.store.slot(slot)
				require.Equal(t, slot <= tt.newest, ok, "slot %d", slot)

				if ok {
					assert.Equal(t, slot == 82, row.Proposed)
					assert.Equal(t, uint64(2), row.Assigned)
					assert.Zero(t, row.Included, "nothing came after slot %d", slot)
				}
			}

			assert.Equal(t, 1, h.node.count("/eth/v1/node/syncing"))
			assert.Equal(t, 1, h.node.count(blockPrefix+slotRoot(82, "")))
		})
	}
}

func TestSlotsCommitteesOfAnOlderEpochNotServed(t *testing.T) {
	h := newHarness(t, 1, slotHosts(), nil)
	chain := newSlotChain(t, h, 70, 82)

	h.node.set(func(n *fakeNode) { n.committeesStatus = map[uint64]int{9: 400} })
	chain.pollAt(t, 82)
	chain.add(83, "", vote(82, true, true))
	chain.pollAt(t, 83)

	_, ok := h.store.slot(79)
	assert.False(t, ok, "nobody can say who had to attest in epoch 9")

	row, _ := h.store.slot(81)
	assert.Equal(t, full(81), row)
	assert.Equal(t, 1, h.node.count("/eth/v1/beacon/states/head/committees?epoch=9"),
		"an older epoch is asked for once")
}

func TestSlotsFleetChangesAndPruning(t *testing.T) {
	store := newFakeStore()
	h := newHarness(t, 1, store, func(o *Options) { o.SlotRetention = 2 * testEpoch })
	chain := newSlotChain(t, h, 70, 82)

	chain.pollAt(t, 82)

	// Without validators only the proposals are known, and no committees
	// are read.
	row, ok := store.slot(80)
	require.True(t, ok)
	assert.Equal(t, fleet.SlotParticipation{Slot: 80, Proposed: true}, row)
	assert.Zero(t, h.node.count("/eth/v1/beacon/states/head/committees"))

	// Rows older than the retention are pruned (slot 82 minus two epochs).
	store.set(func(s *fakeStore) { assert.Equal(t, []uint64{66}, s.slotPrunes) })

	// Hosts register. They are read with the next epoch at the latest, and
	// what is still in memory of the window is counted again with them.
	store.set(func(s *fakeStore) { s.hosts = slotHosts().hosts })

	for slot := uint64(83); slot <= 89; slot++ {
		chain.add(slot, "", vote(slot-1, true, true))
	}

	chain.pollAt(t, 90)

	for slot := uint64(74); slot <= 88; slot++ {
		row, _ = store.slot(slot)
		assert.Equal(t, full(slot), row, "slot %d", slot)
	}

	store.set(func(s *fakeStore) {
		assert.Equal(t, []uint64{66, 74}, s.slotPrunes, "pruned once per epoch")
	})
}

func TestSlotsFailureIsLoggedOnceAndHoldsTheLivenessBack(t *testing.T) {
	h := newHarness(t, 1, slotHosts(), nil)
	chain := newSlotChain(t, h, 70, 82)

	h.node.set(func(n *fakeNode) { n.failPath = blockPrefix + slotRoot(80, "") })
	chain.pollAt(t, 82)
	chain.pollAt(t, 82)

	warnings := 0

	for _, entry := range h.logs.AllEntries() {
		if entry.Message == "Per-slot participation refresh failed" && entry.Level.String() == "warning" {
			warnings++
		}
	}

	assert.Equal(t, 1, warnings)
	assert.Empty(t, h.svc.Health().Error, "the chain view stands")
	assert.Contains(t, h.svc.Health().LivenessError, "reading block")
	assert.False(t, h.svc.Health().LivenessKnown, "no epoch settles from a chain that was not read")

	_, ok := h.store.slot(78)
	assert.False(t, ok, "nothing is stored from a chain that was not read to the end")
	assert.Equal(t, 1, h.node.count(blockPrefix+slotRoot(82, "")), "the blocks read are kept for the retry")

	// The node answers again: the walk goes on where it failed.
	h.node.set(func(n *fakeNode) { n.failPath = "" })
	chain.pollAt(t, 82)

	row, _ := h.store.slot(78)
	assert.Equal(t, full(78), row)
	assert.Equal(t, 1, h.node.count(blockPrefix+slotRoot(82, "")))
	assert.Empty(t, h.svc.Health().LivenessError)
	assert.True(t, h.svc.Health().LivenessKnown)
}

// A fleet that attests stores nothing per host, however large it is, and a
// host is found by its range without looking at every other one.
func TestSlotTrackerStoresOnlyDeviations(t *testing.T) {
	const fleetSize = 1000

	hosts := make([]trackedHost, 0, fleetSize+1)
	committee := make([]uint64, 0, fleetSize)

	for i := range uint64(fleetSize) {
		hosts = append(hosts, trackedHost{name: fmt.Sprintf("node-%04d", i), start: i * 32, end: i*32 + 32})
		// One validator of every host has its duty in the slot.
		committee = append(committee, i*32+5)
	}
	// A range inside another one: both hosts own validator 16005.
	hosts = append(hosts, trackedHost{name: "twin", start: 16000, end: 16010})
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].start < hosts[j].start })

	bits := func(skip ...int) []byte {
		out := make([]byte, fleetSize/8+1)
		for i := range fleetSize {
			if !slices.Contains(skip, i) {
				out[i/8] |= 1 << (i % 8)
			}
		}

		out[fleetSize/8] |= 1 << (fleetSize % 8)

		return out
	}

	tracker := newSlotTracker()
	tracker.blocks = []*chainBlock{
		{slot: 100},
		{slot: 101, attestations: []attestation{{slot: 100, aggregationBits: bits()}}},
	}

	row, _, err := tracker.participation(100, [][]uint64{committee}, hosts)
	require.NoError(t, err)
	assert.Equal(t, fleet.SlotParticipation{
		Slot: 100, Proposed: true, Assigned: fleetSize, Included: fleetSize,
	}, row, "everybody attested: no host is named")

	// Two validators miss: 16005 (hosts node-0500 and twin) and 9989 (node-0312).
	tracker.blocks[1].attestations[0].aggregationBits = bits(500, 312)
	row, _, err = tracker.participation(100, [][]uint64{committee}, hosts)
	require.NoError(t, err)
	assert.Equal(t, uint64(fleetSize-2), row.Included)
	assert.ElementsMatch(t, []fleet.HostSlotParticipation{
		{Host: "node-0312", Assigned: 1}, {Host: "node-0500", Assigned: 1}, {Host: "twin", Assigned: 1},
	}, row.Hosts)

	reach := hostReach(hosts)
	assert.False(t, tracked(hosts, reach, fleetSize*32), "past every range")
	assert.True(t, tracked(hosts, reach, 0))
}
