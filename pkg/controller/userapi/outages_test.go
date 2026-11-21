package userapi

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// outageWorld is a chain of 12 s slots and a window of slots 1000 to 1400.
func outageWorld() (*outageInput, func(slot uint64) time.Time) {
	genesis := controllertest.Epoch.Add(-24 * time.Hour)
	health := &fleet.NetworkHealth{
		GenesisTime: genesis, SecondsPerSlot: 12, SlotsPerEpoch: 32,
		Phases: fleet.SinglePhase(genesis, 12*time.Second),
	}
	at := func(slot uint64) time.Time { return genesis.Add(time.Duration(slot) * 12 * time.Second) }

	return &outageInput{
		health: health, first: 1000, last: 1400, now: at(1400),
		validators: map[string]uint64{"a": 64, "b": 64, "c": 32},
	}, at
}

// missed is a host missing `missed` of `assigned` attestations in each of the slots.
func missed(host string, assigned, missed uint64, slots ...uint64) []fleet.HostSlotRun {
	out := make([]fleet.HostSlotRun, 0, len(slots))
	for _, slot := range slots {
		out = append(out, fleet.HostSlotRun{
			Host: host, First: slot, Last: slot, Slots: 1, Assigned: assigned, Included: assigned - missed,
		})
	}

	return out
}

func slotRange(from, to uint64) []uint64 {
	out := make([]uint64, 0, to-from)
	for slot := from; slot < to; slot++ {
		out = append(out, slot)
	}

	return out
}

func TestOutageSpans(t *testing.T) {
	t.Run("a miss or two is not an outage", func(t *testing.T) {
		in, _ := outageWorld()
		in.runs = append(missed("a", 2, 1, 1100, 1101), missed("b", 2, 2, 1100, 1300, 1390)...)

		assert.Empty(t, outageSpans(in), "two slots in a row, and three that lie more than an epoch apart")
	})

	t.Run("a host that was off and came back", func(t *testing.T) {
		in, at := outageWorld()
		in.runs = missed("a", 2, 2, slotRange(1100, 1150)...)

		assert.Equal(t, []Outage{{
			Hosts: 1, Names: []string{"a"}, From: at(1100), To: at(1150),
			Missed: 100, Assigned: 100, Validators: 64,
		}}, outageSpans(in))
	})

	t.Run("a host that is off for the whole window", func(t *testing.T) {
		in, at := outageWorld()
		// A host with few validators has a duty every few slots only.
		in.runs = missed("c", 1, 1, 1004, 1030, 1061, 1090, 1120, 1150, 1181, 1210, 1240, 1270, 1300, 1330, 1361, 1390)

		got := outageSpans(in)
		require.Len(t, got, 1)
		assert.Equal(t, Outage{
			Hosts: 1, Names: []string{"c"}, From: at(1004), To: at(1391),
			FromStart: true, Open: true, Missed: 14, Assigned: 14, Validators: 32,
		}, got[0])
	})

	t.Run("a host that misses part of its attestations", func(t *testing.T) {
		in, _ := outageWorld()
		in.runs = missed("b", 4, 1, 1200, 1210, 1215, 1240)

		got := outageSpans(in)
		require.Len(t, got, 1)
		assert.Equal(t, uint64(4), got[0].Missed)
		assert.Equal(t, uint64(16), got[0].Assigned, "of the duties in the slots it missed some in")
		assert.False(t, got[0].FromStart || got[0].Open)
	})

	t.Run("a lease explains the misses of its host, and of nobody else", func(t *testing.T) {
		in, at := outageWorld()
		ended := at(1140)
		in.leases = []*fleet.Lease{
			{
				ID: "l1", Host: "a", State: fleet.LeaseReleased, GrantedAt: at(1100), EndedAt: &ended,
				Needs:    []protocol.Need{{Kind: protocol.NeedImage}},
				Rollouts: map[string]string{"n": "r2", "m": "r1"}, Weight: 60,
			},
			// Failed, and still open in the window's eyes: it ended, nobody says when.
			{ID: "l0", Host: "b", State: fleet.LeaseFailed, GrantedAt: at(900)},
			// A reboot and packages that is running.
			{
				ID: "l2", Host: "b", State: fleet.LeaseRunning, GrantedAt: at(1380),
				Needs: []protocol.Need{{Kind: protocol.NeedPackages}, {Kind: protocol.NeedReboot}},
			},
		}
		// Host a misses during its lease and for a few slots after it; host
		// b misses at the same time, with no lease then.
		in.runs = append(missed("a", 2, 2, slotRange(1102, 1150)...), missed("b", 2, 2, slotRange(1102, 1110)...)...)

		got := outageSpans(in)
		require.Len(t, got, 3)

		assert.Equal(t, Outage{
			Hosts: 1, Names: []string{"a"}, From: at(1100), To: at(1140),
			Lease: "l1", Work: "image", Rollout: "r1", Missed: 96, Assigned: 96, Validators: 64,
		}, got[0], "the misses until an epoch after the lease are the lease's")
		assert.Equal(t, Outage{
			Hosts: 1, Names: []string{"b"}, From: at(1102), To: at(1110),
			Missed: 16, Assigned: 16, Validators: 64,
		}, got[1])
		assert.Equal(t, Outage{
			Hosts: 1, Names: []string{"b"}, From: at(1380), To: at(1400), Open: true,
			Lease: "l2", Work: workMixed, Validators: 64,
		}, got[2])
	})
}

// However many hosts were off, the answer is bounded: spans of one kind that
// overlap stand as one, with the number of hosts and some of their names.
func TestCapOutages(t *testing.T) {
	in, at := outageWorld()

	// A thousand hosts rolled a hundred at a time, each for 30 slots, and
	// forty hosts that are off for the whole window.
	for i := range 1000 {
		wave := uint64(i / 100)
		start, end := at(1000+wave*35), at(1000+wave*35+30)
		in.leases = append(in.leases, &fleet.Lease{
			ID: fmt.Sprintf("l%04d", i), Host: fmt.Sprintf("node-%04d", i), State: fleet.LeaseReleased,
			GrantedAt: start, EndedAt: &end, Needs: []protocol.Need{{Kind: protocol.NeedImage}},
			Rollouts: map[string]string{"n": "rollout-1"}, Weight: 32,
		})
	}

	for i := range 40 {
		in.runs = append(in.runs,
			missed(fmt.Sprintf("dead-%02d", i), 1, 1, slotRange(1000, 1400)...)...)
	}

	spans := outageSpans(in)
	require.Len(t, spans, 1040)

	got := capOutages(spans, maxOutages, 400*12*time.Second)
	require.Len(t, got, 11, "ten waves and the hosts that are down")

	hosts := 0
	for i := range got {
		hosts += got[i].Hosts
		assert.LessOrEqual(t, len(got[i].Names), maxOutageNames)
	}

	assert.Equal(t, 1040, hosts, "nobody is lost in the merge")

	down := got[0]
	assert.Equal(t, 40, down.Hosts)
	assert.True(t, down.FromStart && down.Open)
	assert.Empty(t, down.Work)
	assert.Equal(t, uint64(40*400), down.Missed)

	wave := got[1]
	assert.Equal(t, 100, wave.Hosts)
	assert.Equal(t, "image", wave.Work)
	assert.Equal(t, "rollout-1", wave.Rollout)
	assert.Empty(t, wave.Lease, "a span of several hosts is not one lease")
	assert.Equal(t, uint64(3200), wave.Validators)
	assert.Equal(t, at(1000), wave.From)
	assert.Equal(t, at(1030), wave.To)

	// A few spans stay as they are.
	few := outageSpans(in)[:5]
	assert.Equal(t, few, capOutages(few, maxOutages, time.Hour))
}

// The route puts it together: the leases and the misses of the window, for a
// login only.
func TestSlotHistoryOutages(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	genesis := controllertest.Epoch.Add(-24 * time.Hour)
	at := func(slot uint64) time.Time { return genesis.Add(time.Duration(slot) * 12 * time.Second) }
	ended := at(7180)

	h.store.Leases = append(h.store.Leases, &fleet.Lease{
		ID: "lease-roll", Host: "lighthouse-geth-2", State: fleet.LeaseReleased,
		GrantedAt: at(7160), EndedAt: &ended, Needs: []protocol.Need{{Kind: protocol.NeedReboot}},
	})

	h.store.Slots = nil
	for slot := uint64(7155); slot < 7190; slot++ {
		h.store.Slots = append(h.store.Slots, fleet.SlotParticipation{
			Slot: slot, Proposed: true, Assigned: 4, Included: 0,
			Hosts: []fleet.HostSlotParticipation{
				{Host: "lighthouse-geth-1", Assigned: 2},
				{Host: "lighthouse-geth-2", Assigned: 2},
			},
		})
	}

	got := controllertest.DecodeJSON[SlotHistory](t, h.get("/api/v1/slots?window=600"))

	require.Len(t, got.Outages, 3)
	assert.Equal(t, 2, got.OutageHosts)

	// lighthouse-geth-1 was off without a lease; lighthouse-geth-2 was off
	// before its reboot began, and then for the reboot.
	assert.Equal(t, []string{"lighthouse-geth-1"}, got.Outages[0].Names)
	assert.Empty(t, got.Outages[0].Lease)
	assert.Equal(t, uint64(100), got.Outages[0].Validators)
	assert.Equal(t, at(7155), got.Outages[1].From)
	assert.Equal(t, at(7160), got.Outages[1].To)
	assert.Equal(t, "lease-roll", got.Outages[2].Lease)
	assert.Equal(t, "reboot", got.Outages[2].Work)
	assert.Equal(t, uint64(60), got.Outages[2].Missed)

	rec := h.get("/api/v1/public/slots?window=600")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "lighthouse-geth")
}
