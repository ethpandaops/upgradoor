package beacon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawSpec builds spec values the way a node sends them: decimal strings.
func rawSpec(t *testing.T, values map[string]any) map[string]json.RawMessage {
	t.Helper()

	spec := make(map[string]json.RawMessage, len(values))

	for key, value := range values {
		raw, err := json.Marshal(value)
		require.NoError(t, err)

		spec[key] = raw
	}

	return spec
}

func TestChainTiming(t *testing.T) {
	const farFuture = "18446744073709551615"

	type probe struct {
		offset time.Duration
		slot   uint64
		epoch  uint64
	}

	tests := []struct {
		name       string
		spec       map[string]any
		wantErr    string
		wantPhases int
		probes     []probe
		// epochStarts maps an epoch to its start offset from genesis.
		epochStarts map[uint64]time.Duration
	}{
		{
			name:       "seconds per slot",
			spec:       map[string]any{"SECONDS_PER_SLOT": "12", "SLOTS_PER_EPOCH": "32"},
			wantPhases: 1,
			probes: []probe{
				{offset: -time.Hour, slot: 0, epoch: 0},
				{offset: 0, slot: 0, epoch: 0},
				{offset: 12*time.Second - time.Nanosecond, slot: 0, epoch: 0},
				{offset: 12 * time.Second, slot: 1, epoch: 0},
				{offset: 384 * time.Second, slot: 32, epoch: 1},
				{offset: 10*384*time.Second + 100*time.Second, slot: 328, epoch: 10},
			},
			epochStarts: map[uint64]time.Duration{0: 0, 1: 384 * time.Second, 10: 3840 * time.Second},
		},
		{
			name: "slot duration in milliseconds wins",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOT_DURATION_MS": "6000", "SLOTS_PER_EPOCH": "32",
			},
			wantPhases: 1,
			probes: []probe{
				{offset: 6 * time.Second, slot: 1, epoch: 0},
				{offset: 384 * time.Second, slot: 64, epoch: 2},
			},
			epochStarts: map[uint64]time.Duration{1: 192 * time.Second},
		},
		{
			name:       "sub-second slots and bare numbers",
			spec:       map[string]any{"SLOT_DURATION_MS": 500, "SLOTS_PER_EPOCH": 4},
			wantPhases: 1,
			probes:     []probe{{offset: 3250 * time.Millisecond, slot: 6, epoch: 1}},
		},
		{
			name: "fork changes the slot duration",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOT_DURATION_MS": "12000", "SLOTS_PER_EPOCH": "32",
				"SLOT_DURATION_MS_HEZE": "9000", "HEZE_FORK_EPOCH": "5",
				"HEZE_FORK_VERSION": "0x90954630",
			},
			wantPhases: 2,
			probes: []probe{
				{offset: 1919 * time.Second, slot: 159, epoch: 4},
				{offset: 1920 * time.Second, slot: 160, epoch: 5},
				{offset: 1920*time.Second + 9*time.Second, slot: 161, epoch: 5},
				// Observed on heze-devnet-3: 2068879 s after genesis.
				{offset: 2068879 * time.Second, slot: 229822, epoch: 7181},
			},
			epochStarts: map[uint64]time.Duration{
				4: 1536 * time.Second,
				5: 1920 * time.Second,
				6: 1920*time.Second + 288*time.Second,
			},
		},
		{
			name: "fork at genesis replaces the base duration",
			spec: map[string]any{
				"SLOT_DURATION_MS": "12000", "SLOTS_PER_EPOCH": "32",
				"SLOT_DURATION_MS_HEZE": "9000", "HEZE_FORK_EPOCH": "0",
			},
			wantPhases: 1,
			probes:     []probe{{offset: 18 * time.Second, slot: 2, epoch: 0}},
		},
		{
			name: "two forks in order",
			spec: map[string]any{
				"SLOT_DURATION_MS": "12000", "SLOTS_PER_EPOCH": "10",
				"SLOT_DURATION_MS_BBB": "4000", "BBB_FORK_EPOCH": "3",
				"SLOT_DURATION_MS_AAA": "6000", "AAA_FORK_EPOCH": "1",
			},
			wantPhases: 3,
			probes: []probe{
				{offset: 120 * time.Second, slot: 10, epoch: 1},
				{offset: 239 * time.Second, slot: 29, epoch: 2},
				{offset: 240 * time.Second, slot: 30, epoch: 3},
				{offset: 248 * time.Second, slot: 32, epoch: 3},
			},
			epochStarts: map[uint64]time.Duration{3: 240 * time.Second, 4: 280 * time.Second},
		},
		{
			name: "unscheduled and unknown forks are ignored",
			spec: map[string]any{
				"SLOT_DURATION_MS": "12000", "SLOTS_PER_EPOCH": "32",
				"SLOT_DURATION_MS_HEZE": "9000", "HEZE_FORK_EPOCH": farFuture,
				"SLOT_DURATION_MS_NEXT": "6000",
			},
			wantPhases: 1,
			probes:     []probe{{offset: 384 * time.Second, slot: 32, epoch: 1}},
		},
		{
			name: "fork beyond the clock range is ignored",
			spec: map[string]any{
				"SLOT_DURATION_MS": "12000", "SLOTS_PER_EPOCH": "32",
				"SLOT_DURATION_MS_HEZE": "9000", "HEZE_FORK_EPOCH": "18446744073709551614",
			},
			wantPhases: 1,
			probes:     []probe{{offset: 384 * time.Second, slot: 32, epoch: 1}},
		},
		{
			name:    "no slots per epoch",
			spec:    map[string]any{"SECONDS_PER_SLOT": "12"},
			wantErr: "SLOTS_PER_EPOCH",
		},
		{
			name:    "zero slots per epoch",
			spec:    map[string]any{"SECONDS_PER_SLOT": "12", "SLOTS_PER_EPOCH": "0"},
			wantErr: "SLOTS_PER_EPOCH",
		},
		{
			name:    "no slot duration",
			spec:    map[string]any{"SLOTS_PER_EPOCH": "32"},
			wantErr: "SECONDS_PER_SLOT",
		},
		{
			name:    "slot duration is not a number",
			spec:    map[string]any{"SLOT_DURATION_MS": "fast", "SLOTS_PER_EPOCH": "32"},
			wantErr: "SLOT_DURATION_MS is not an unsigned integer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timing, err := newChainTiming(testGenesis, rawSpec(t, tt.spec))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Len(t, timing.phases, tt.wantPhases)

			for _, p := range tt.probes {
				now := testGenesis.Add(p.offset)
				assert.Equal(t, p.slot, timing.slotAt(now), "slot at %s", p.offset)
				assert.Equal(t, p.epoch, timing.epochAt(now), "epoch at %s", p.offset)
			}

			for epoch, offset := range tt.epochStarts {
				assert.Equal(t, testGenesis.Add(offset), epochStart(timing, epoch),
					"start of epoch %d", epoch)
			}
		})
	}
}

func TestWholeSeconds(t *testing.T) {
	tests := []struct {
		slot time.Duration
		want uint64
	}{
		{slot: 12 * time.Second, want: 12},
		{slot: 9 * time.Second, want: 9},
		{slot: 6500 * time.Millisecond, want: 7},
		{slot: 6400 * time.Millisecond, want: 6},
		{slot: 250 * time.Millisecond, want: 1},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, wholeSeconds(tt.slot), tt.slot.String())
	}
}

// TestPublishedPhasesPlaceEpochsLikeTheService: what the service publishes
// for the scheduler and the API places every epoch where the service's own
// timing does, across forks that change the slot duration, one of them to a
// slot that is not a whole number of seconds.

// epochStart is where the timing places the start of an epoch.
func epochStart(timing *chainTiming, epoch uint64) time.Time {
	start, _ := timing.clock.EpochStart(epoch)

	return start
}
