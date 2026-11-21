package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// The fixture's chain: 12 s slots, 32 per epoch, genesis 24 h before the
// test clock, so the clock is in slot 7200.
const slotNow = 7200

func slotRow(slot, aAssigned, aIncluded, bAssigned, bIncluded uint64, proposed bool) fleet.SlotParticipation {
	row := fleet.SlotParticipation{
		Slot: slot, Proposed: proposed,
		Assigned: aAssigned + bAssigned, Included: aIncluded + bIncluded,
	}

	if aAssigned > 0 {
		row.Hosts = append(row.Hosts,
			fleet.HostSlotParticipation{Host: "a", Assigned: aAssigned, Included: aIncluded})
	}

	if bAssigned > 0 {
		row.Hosts = append(row.Hosts,
			fleet.HostSlotParticipation{Host: "b", Assigned: bAssigned, Included: bIncluded})
	}

	return row
}

func TestSlotHistoryPerSlot(t *testing.T) {
	h := newHarness(t, nil)
	leakFixture(t, h)

	// A 10 minute window is 50 slots: slots 7150 to 7200, one point each.
	h.store.slots = []fleet.SlotParticipation{
		slotRow(7149, 2, 2, 2, 2, true), // before the window
		slotRow(7150, 2, 2, 2, 2, true),
		slotRow(7151, 2, 0, 1, 1, true),
		slotRow(7152, 3, 3, 0, 0, false), // no block; b has no duty
		// 7153 is not stored: the controller was not watching.
		slotRow(7198, 2, 2, 2, 1, true),
	}

	rec := h.get("/api/v1/slots?window=600")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := decodeJSON[SlotHistory](t, rec)
	genesis := testEpoch.Add(-24 * time.Hour)

	assert.Equal(t, uint64(7150), got.FirstSlot)
	assert.Equal(t, genesis.Add(7150*12*time.Second), got.FirstTime)
	assert.Equal(t, uint64(12_000), got.SlotMs)
	assert.Equal(t, uint64(32), got.SlotsPerEpoch)
	assert.Equal(t, uint64(1), got.BucketSlots)
	assert.InDelta(t, fleet.DefaultSettings().MinParticipationPercent, got.MinParticipationPercent, 0)
	assert.InDelta(t, 384, got.EpochSeconds, 0)

	require.Len(t, got.Recorded, slotNow-7150+1, "one point per slot up to the slot the clock is in")
	assert.Equal(t, []uint64{1, 1, 1, 0}, got.Recorded[:4])
	assert.Equal(t, []uint64{0, 0, 1, 0}, got.Missed[:4])
	assert.Equal(t, []uint64{4, 3, 3, 0}, got.Assigned[:4])
	assert.Equal(t, []uint64{4, 1, 3, 0}, got.Included[:4])
	assert.Equal(t, uint64(1), got.Recorded[48])
	assert.Equal(t, uint64(3), got.Included[48])
	assert.Zero(t, got.Recorded[50], "the slot the clock is in has not settled")
	assert.Nil(t, got.WorstAssigned, "a point that is one slot is its own worst")
	assert.Nil(t, got.WorstIncluded)

	// No row per host: two slots with a miss are not an outage, so nothing
	// is said about a host at all.
	assert.Empty(t, got.Outages)
	assert.Zero(t, got.OutageHosts)
	assert.Equal(t, uint64(outageMinSlots), got.OffSlots)

	// The public route carries the same sums and nothing about a host.
	body := h.get("/api/v1/public/slots?window=600").Body.String()
	public := decodeJSON[SlotHistory](t, h.get("/api/v1/public/slots?window=600"))
	assert.NotContains(t, body, `"outages"`)
	assert.NotContains(t, body, `"offSlots"`)

	got.OffSlots = 0
	assert.Equal(t, got, public)
}

func TestSlotHistoryBuckets(t *testing.T) {
	h := newHarness(t, nil)
	leakFixture(t, h)

	// 12 h are 3600 slots: more than an answer carries points, so they come
	// in buckets of 16 slots, aligned to the slot number. Slots 3600 to 3615
	// are point 0, 3616 to 3631 point 1.
	h.store.slots = []fleet.SlotParticipation{
		slotRow(3599, 4, 0, 4, 0, true), // the bucket before the window
		slotRow(3600, 4, 4, 4, 4, true),
		slotRow(3601, 4, 4, 0, 0, true),
		slotRow(3602, 2, 0, 2, 1, false), // the worst slot of the bucket: 1 of 4
		slotRow(3603, 0, 0, 0, 0, true),  // no duty: not the worst, whatever its share
		slotRow(3615, 4, 2, 4, 4, true),
		slotRow(3616, 4, 4, 4, 4, true),
	}

	// A window longer than the longest is the longest.
	got := decodeJSON[SlotHistory](t, h.get("/api/v1/slots?window=999999"))

	assert.Equal(t, uint64(16), got.BucketSlots)
	assert.Equal(t, uint64(3600), got.FirstSlot)
	require.Len(t, got.Recorded, (slotNow-3600)/16+1)
	assert.LessOrEqual(t, len(got.Recorded), maxSlotPoints+1)

	assert.Equal(t, []uint64{5, 1, 0}, got.Recorded[:3])
	assert.Equal(t, []uint64{1, 0, 0}, got.Missed[:3])
	assert.Equal(t, []uint64{24, 8, 0}, got.Assigned[:3])
	assert.Equal(t, []uint64{19, 8, 0}, got.Included[:3])
	assert.Equal(t, []uint64{4, 8, 0}, got.WorstAssigned[:3], "the dip stays visible in the bucket")
	assert.Equal(t, []uint64{1, 8, 0}, got.WorstIncluded[:3])

	// Two slots with misses of a host are not an outage.
	assert.Empty(t, got.Outages)

	// Which host was off is not public.
	assert.NotContains(t, h.get("/api/v1/public/slots?window=999999").Body.String(), `"outages"`)
}

func TestSlotHistoryWindow(t *testing.T) {
	h := newHarness(t, nil)
	leakFixture(t, h)

	for _, window := range []string{"0", "59", "-600", "abc", "1h"} {
		assert.Equal(t, http.StatusBadRequest, h.get("/api/v1/public/slots?window="+window).Code, window)
	}

	// The default is three hours: 900 slots in buckets of 4.
	got := decodeJSON[SlotHistory](t, h.get("/api/v1/public/slots"))
	assert.Equal(t, uint64(4), got.BucketSlots)
	assert.Equal(t, uint64(slotNow-900), got.FirstSlot)

	// A chain younger than the window begins at its first slot.
	health := h.beacon.health
	health.Phases = fleet.SinglePhase(testEpoch.Add(-5*time.Minute), 12*time.Second)
	h.beacon.health = health

	got = decodeJSON[SlotHistory](t, h.get("/api/v1/public/slots?window=3600"))
	assert.Zero(t, got.FirstSlot)
	assert.Equal(t, uint64(1), got.BucketSlots)
	assert.Len(t, got.Recorded, 26)

	// Without the chain's timing nothing can be placed: empty lists, not null.
	health.Phases = nil
	h.beacon.health = health

	rec := h.get("/api/v1/public/slots")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"recorded":[]`)
	assert.Contains(t, rec.Body.String(), `"included":[]`)
}
