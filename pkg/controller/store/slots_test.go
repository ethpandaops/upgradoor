package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

func TestSlotParticipation(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			empty, err := s.ListSlotParticipation(ctx, 0)
			require.NoError(t, err)
			assert.NotNil(t, empty)
			assert.Empty(t, empty)

			require.NoError(t, s.SaveSlotParticipation(ctx, nil), "no rows is a no-op")
			require.NoError(t, s.PruneSlotParticipation(ctx, 100), "pruning nothing is fine")

			// Slot 11 has no block; node-b has no duty in slot 12. Hosts need
			// not be registered. Only a host that missed attestations in a
			// slot gets a row: in slot 10 and 12 everybody attested.
			require.NoError(t, s.SaveSlotParticipation(ctx, []fleet.SlotParticipation{
				{Slot: 10, Proposed: true, Assigned: 8, Included: 8, Hosts: []fleet.HostSlotParticipation{
					{Host: "node-a", Assigned: 5, Included: 5},
					{Host: "node-b", Assigned: 3, Included: 3},
				}},
				{Slot: 11, Assigned: 8, Included: 2, Hosts: []fleet.HostSlotParticipation{
					{Host: "node-a", Assigned: 6, Included: 2},
					{Host: "node-b", Assigned: 2, Included: 0},
				}},
				{Slot: 12, Proposed: true, Assigned: 4, Included: 4, Hosts: []fleet.HostSlotParticipation{
					{Host: "node-a", Assigned: 4, Included: 4},
				}},
			}))

			// A late inclusion raises slot 11, and node-b left the fleet: the
			// slot's host rows are replaced, not merged.
			require.NoError(t, s.SaveSlotParticipation(ctx, []fleet.SlotParticipation{
				{Slot: 11, Assigned: 6, Included: 5, Hosts: []fleet.HostSlotParticipation{
					{Host: "node-a", Assigned: 6, Included: 5},
				}},
			}))

			rows, err := s.ListSlotParticipation(ctx, 11)
			require.NoError(t, err)
			assert.Equal(t, []fleet.SlotParticipation{
				{Slot: 11, Assigned: 6, Included: 5},
				{Slot: 12, Proposed: true, Assigned: 4, Included: 4},
			}, rows, "from the slot on, oldest first, without hosts")

			perSlot, err := s.SumHostSlotParticipation(ctx, 0, 1)
			require.NoError(t, err)
			assert.Equal(t, []fleet.HostSlotBucket{
				{Bucket: 11, Host: "node-a", Assigned: 6, Included: 5},
			}, perSlot, "the hosts that deviated, and no others")
			assert.Equal(t, 1, countRows(t, s, "slot_participation_hosts"))

			// Buckets of 4 slots: slots 10 and 11 are bucket 2, slot 12 is
			// bucket 3; slot 10 lies before the window asked for.
			summed, err := s.SumHostSlotParticipation(ctx, 11, 4)
			require.NoError(t, err)
			assert.Equal(t, []fleet.HostSlotBucket{
				{Bucket: 2, Host: "node-a", Assigned: 6, Included: 5},
			}, summed)

			none, err := s.SumHostSlotParticipation(ctx, 0, 0)
			require.NoError(t, err)
			assert.Empty(t, none, "a bucket of no slots holds nothing")

			// A late inclusion that completes a slot takes its host row away.
			require.NoError(t, s.SaveSlotParticipation(ctx, []fleet.SlotParticipation{
				{Slot: 12, Proposed: true, Assigned: 4, Included: 3, Hosts: []fleet.HostSlotParticipation{
					{Host: "node-a", Assigned: 4, Included: 3},
				}},
			}))
			require.NoError(t, s.PruneSlotParticipation(ctx, 12))

			rows, err = s.ListSlotParticipation(ctx, 0)
			require.NoError(t, err)
			assert.Equal(t, []fleet.SlotParticipation{
				{Slot: 12, Proposed: true, Assigned: 4, Included: 3},
			}, rows)

			perSlot, err = s.SumHostSlotParticipation(ctx, 0, 1)
			require.NoError(t, err)
			assert.Equal(t, []fleet.HostSlotBucket{
				{Bucket: 12, Host: "node-a", Assigned: 4, Included: 3},
			}, perSlot, "the host rows go with their slot")

			require.NoError(t, s.SaveSlotParticipation(ctx, []fleet.SlotParticipation{
				{Slot: 12, Proposed: true, Assigned: 4, Included: 4},
			}))
			assert.Zero(t, countRows(t, s, "slot_participation_hosts"))
		})
	}
}

// Runs are found in the database: rows of a host no more than the gap apart
// are one run, and the hosts asked for come slot by slot.
func TestSumHostSlotRuns(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			missed := func(slot uint64, hosts ...string) fleet.SlotParticipation {
				row := fleet.SlotParticipation{Slot: slot, Assigned: 10}
				for _, h := range hosts {
					row.Hosts = append(row.Hosts, fleet.HostSlotParticipation{Host: h, Assigned: 2, Included: 1})
				}

				return row
			}

			// node-a: 10, 12, 14 and, after a long gap, 30, 31. node-b: 11, 13.
			require.NoError(t, s.SaveSlotParticipation(ctx, []fleet.SlotParticipation{
				missed(10, "node-a"), missed(11, "node-b"), missed(12, "node-a"), missed(13, "node-b"),
				missed(14, "node-a"), missed(30, "node-a"), missed(31, "node-a"),
			}))

			runs, err := s.SumHostSlotRuns(ctx, 0, 100, 4, 1, nil)
			require.NoError(t, err)
			assert.Equal(t, []fleet.HostSlotRun{
				{Host: "node-a", First: 10, Last: 14, Slots: 3, Assigned: 6, Included: 3},
				{Host: "node-a", First: 30, Last: 31, Slots: 2, Assigned: 4, Included: 2},
				{Host: "node-b", First: 11, Last: 13, Slots: 2, Assigned: 4, Included: 2},
			}, runs)

			runs, err = s.SumHostSlotRuns(ctx, 12, 30, 4, 2, []string{"node-b"})
			require.NoError(t, err)
			assert.Equal(t, []fleet.HostSlotRun{
				{Host: "node-a", First: 12, Last: 14, Slots: 2, Assigned: 4, Included: 2},
				{Host: "node-b", First: 13, Last: 13, Slots: 1, Assigned: 2, Included: 1},
			}, runs, "within the window, two slots at least; node-b slot by slot")
		})
	}
}

// Twelve hours of a fleet of 1000 hosts of which 50 are down the whole time
// and the rest miss a slot now and then: the runs are a few hundred rows,
// not the 200 000 the window holds.
func TestSumHostSlotRunsOfALargeFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("large fleet")
	}

	ctx := context.Background()
	s := newFileStore(t)

	const (
		slots = 3600
		dead  = 50
		hosts = 1000
	)

	batch := make([]fleet.SlotParticipation, 0, 100)

	for slot := uint64(1); slot <= slots; slot++ {
		row := fleet.SlotParticipation{Slot: slot, Proposed: true, Assigned: 1000, Included: 900}
		for h := range dead {
			row.Hosts = append(row.Hosts, fleet.HostSlotParticipation{Host: fmt.Sprintf("dead-%02d", h), Assigned: 1})
		}

		// Every live host misses once in a while.
		for h := dead + int(slot%97); h < hosts; h += 97 {
			row.Hosts = append(row.Hosts, fleet.HostSlotParticipation{
				Host: fmt.Sprintf("node-%04d", h), Assigned: 2, Included: 1,
			})
		}

		batch = append(batch, row)
		if len(batch) == cap(batch) {
			require.NoError(t, s.SaveSlotParticipation(ctx, batch))
			batch = batch[:0]
		}
	}

	start := time.Now()
	perSlot, err := s.SumHostSlotParticipation(ctx, 1, 1)
	require.NoError(t, err)

	old := time.Since(start)

	start = time.Now()
	runs, err := s.SumHostSlotRuns(ctx, 1, slots, 32, 3, nil)
	require.NoError(t, err)

	took := time.Since(start)

	long := 0

	for _, r := range runs {
		if r.Slots == slots {
			long++
		}
	}

	assert.Equal(t, dead, long, "a dead host is one run")
	assert.Less(t, len(runs), len(perSlot)/50)
	t.Logf("%d rows slot by slot in %s; %d runs in %s", len(perSlot), old, len(runs), took)
}
