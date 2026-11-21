package fleet

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlotTimes(t *testing.T) {
	genesis := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 8 slots per epoch; 12 s slots until epoch 2, 4 s slots from there on.
	health := NetworkHealth{
		SlotsPerEpoch: 8,
		Phases: []SlotPhase{
			{Epoch: 0, Start: genesis, SlotMs: 12_000},
			{Epoch: 2, Start: genesis.Add(192 * time.Second), SlotMs: 4_000},
		},
	}

	tests := []struct {
		name  string
		slot  uint64
		start time.Duration
	}{
		{name: "genesis", slot: 0, start: 0},
		{name: "inside the first phase", slot: 9, start: 108 * time.Second},
		{name: "first slot of the second phase", slot: 16, start: 192 * time.Second},
		{name: "inside the second phase", slot: 27, start: 236 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, ok := health.SlotStart(tt.slot)
			require.True(t, ok)
			assert.Equal(t, genesis.Add(tt.start), start)

			for _, into := range []time.Duration{0, time.Millisecond, 3999 * time.Millisecond} {
				slot, ok := health.SlotAt(start.Add(into))
				require.True(t, ok)
				assert.Equal(t, tt.slot, slot, "slot at %s into it", into)
			}
		})
	}

	_, ok := health.SlotAt(genesis.Add(-time.Second))
	assert.False(t, ok, "no slot before genesis")

	length, ok := health.SlotDuration(5)
	require.True(t, ok)
	assert.Equal(t, 4*time.Second, length)

	var unknown NetworkHealth

	_, ok = unknown.SlotStart(3)
	assert.False(t, ok, "no timing, no slot time")

	_, ok = unknown.SlotAt(genesis)
	assert.False(t, ok)
}
