package fleet

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// TestEpochTimes: epochs are placed by the chain's slot phases, exactly, also
// across a fork that changes the slot duration and with slots that are not
// whole seconds.
func TestEpochTimes(t *testing.T) {
	genesis := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 12 s slots until epoch 100, 6 s until epoch 200, then 4.5 s; 32 slots
	// per epoch.
	fork1 := genesis.Add(100 * 384 * time.Second)
	fork2 := fork1.Add(100 * 192 * time.Second)
	n := &NetworkHealth{
		SlotsPerEpoch: 32,
		Phases: []SlotPhase{
			{Epoch: 0, Start: genesis, SlotMs: 12000},
			{Epoch: 100, Start: fork1, SlotMs: 6000},
			{Epoch: 200, Start: fork2, SlotMs: 4500},
		},
	}

	for _, tt := range []struct {
		epoch uint64
		start time.Time
	}{
		{0, genesis},
		{1, genesis.Add(384 * time.Second)},
		{99, fork1.Add(-384 * time.Second)},
		{100, fork1},
		{101, fork1.Add(192 * time.Second)},
		{199, fork2.Add(-192 * time.Second)},
		{200, fork2},
		{203, fork2.Add(3 * 144 * time.Second)},
	} {
		start, ok := n.EpochStart(tt.epoch)
		require.True(t, ok, "epoch %d", tt.epoch)
		assert.Equal(t, tt.start, start, "start of epoch %d", tt.epoch)

		at, ok := n.EpochAt(tt.start)
		require.True(t, ok)
		assert.Equal(t, tt.epoch, at, "the epoch at its own start")

		at, ok = n.EpochAt(tt.start.Add(-time.Millisecond))
		if tt.epoch == 0 {
			assert.False(t, ok, "before genesis")
		} else {
			require.True(t, ok)
			assert.Equal(t, tt.epoch-1, at, "a millisecond earlier")
		}
	}

	// A span of epochs across a fork: one long epoch and two short ones.
	span, ok := n.EpochSpan(fork1.Add(-10*time.Second), 3)
	require.True(t, ok)
	assert.Equal(t, (384+192+192)*time.Second, span)

	span, ok = n.EpochSpan(fork2.Add(time.Second), 2)
	require.True(t, ok)
	assert.Equal(t, 288*time.Second, span)

	n.CurrentEpoch = 150
	assert.Equal(t, 192*time.Second, n.EpochDuration())
	n.CurrentEpoch = 250
	assert.Equal(t, 144*time.Second, n.EpochDuration())

	// What cannot be placed is not placed.
	for name, bad := range map[string]*NetworkHealth{
		"no phases":          {SlotsPerEpoch: 32},
		"no slots per epoch": {Phases: SinglePhase(genesis, 12*time.Second)},
		"a slot of nothing":  {SlotsPerEpoch: 32, Phases: []SlotPhase{{Start: genesis}}},
	} {
		_, placed := bad.EpochStart(4)
		assert.False(t, placed, name)
		assert.Zero(t, bad.EpochDuration(), name)

		_, placed = bad.EpochSpan(genesis.Add(time.Hour), 1)
		assert.False(t, placed, name)

		_, placed = bad.TimingStart()
		assert.False(t, placed, name)
	}

	// An epoch before the first known phase has no place.
	late := &NetworkHealth{
		SlotsPerEpoch: 32, CurrentEpoch: 4, Phases: []SlotPhase{{Epoch: 5, Start: genesis, SlotMs: 12000}},
	}
	_, ok = late.EpochStart(4)
	assert.False(t, ok)
	assert.Zero(t, late.EpochDuration())

	at, ok := late.EpochAt(genesis)
	require.True(t, ok)
	assert.Equal(t, uint64(5), at)
}

// TestEpochSpanBeforeTheFirstPhase: a time before genesis lies in no epoch;
// a span of epochs asked for from there counts from genesis.
func TestEpochSpanBeforeTheFirstPhase(t *testing.T) {
	genesis := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := &NetworkHealth{
		SlotsPerEpoch: 32,
		Phases: []SlotPhase{
			{Epoch: 0, Start: genesis, SlotMs: 12000},
			{Epoch: 1, Start: genesis.Add(384 * time.Second), SlotMs: 6000},
		},
	}

	_, ok := n.EpochAt(genesis.Add(-time.Nanosecond))
	assert.False(t, ok, "no epoch before genesis")

	start, ok := n.TimingStart()
	require.True(t, ok)
	assert.Equal(t, genesis, start)

	for _, before := range []time.Duration{time.Nanosecond, 3 * time.Minute, 400 * 24 * time.Hour} {
		span, placed := n.EpochSpan(genesis.Add(-before), 2)
		require.True(t, placed, "%s before genesis", before)
		assert.Equal(t, (384+192)*time.Second, span, "the first two epochs")
	}

	span, ok := n.EpochSpan(genesis.Add(-time.Hour), 0)
	require.True(t, ok)
	assert.Zero(t, span)
}

// A timing is valid as a whole or places no epoch at all: one check, when it
// is read, and no part of it is used when a phase is wrong.
func TestValidateTiming(t *testing.T) {
	genesis := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fork := genesis.Add(10 * 384 * time.Second)

	tests := []struct {
		name   string
		slots  uint64
		phases []SlotPhase
		want   error
		reason string
	}{
		{name: "one phase", slots: 32, phases: SinglePhase(genesis, 12*time.Second)},
		{
			name:  "a fork that follows from the phase before it",
			slots: 32,
			phases: []SlotPhase{
				{Epoch: 0, Start: genesis, SlotMs: 12000}, {Epoch: 10, Start: fork, SlotMs: 6000},
			},
		},
		{name: "no phases", slots: 32, want: ErrNoTiming},
		{name: "no slots per epoch", phases: SinglePhase(genesis, 12*time.Second), want: ErrNoTiming},
		{
			name: "a slot without a duration", slots: 32,
			phases: []SlotPhase{{Epoch: 0, Start: genesis}},
			want:   ErrInvalidTiming, reason: "phase 0 (from epoch 0) has slots of no duration",
		},
		{
			name: "phases out of order", slots: 32,
			phases: []SlotPhase{
				{Epoch: 10, Start: fork, SlotMs: 6000}, {Epoch: 0, Start: genesis, SlotMs: 12000},
			},
			want: ErrInvalidTiming, reason: "phase 1 begins at epoch 0, not after epoch 10",
		},
		{
			name: "a phase that does not start where the one before it ends", slots: 32,
			phases: []SlotPhase{
				{Epoch: 0, Start: genesis, SlotMs: 12000}, {Epoch: 10, Start: fork.Add(time.Second), SlotMs: 6000},
			},
			want: ErrInvalidTiming, reason: "phase 1 (from epoch 10) starts at 2026-01-01T01:04:01Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTiming(tt.slots, tt.phases)
			n := &NetworkHealth{SlotsPerEpoch: tt.slots, Phases: tt.phases, CurrentEpoch: 3}

			_, placed := n.EpochStart(3)
			_, told := n.EpochAt(genesis.Add(time.Hour))
			_, spanned := n.EpochSpan(genesis, 1)
			_, started := n.TimingStart()

			if tt.want == nil {
				require.NoError(t, err)
				assert.True(t, placed && told && spanned && started)
				assert.Positive(t, n.EpochDuration())

				return
			}

			require.ErrorIs(t, err, tt.want)
			assert.Contains(t, err.Error(), tt.reason)
			assert.False(t, placed || told || spanned || started, "an invalid timing places nothing")
			assert.Zero(t, n.EpochDuration())
		})
	}
}

// A host the controller asked to sync rarely is stale only after missing two
// of those syncs.
func TestHostStaleAfter(t *testing.T) {
	now := time.Unix(10_000, 0)

	for _, tc := range []struct {
		name     string
		interval time.Duration
		silent   time.Duration
		want     bool
	}{
		{"short interval, within the setting", 30 * time.Second, 110 * time.Second, false},
		{"short interval, past the setting", 30 * time.Second, 130 * time.Second, true},
		{"long interval, past the setting", 3 * time.Minute, 5 * time.Minute, false},
		{"long interval, two missed", 3 * time.Minute, 7 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Host{LastSeen: now.Add(-tc.silent), SyncInterval: tc.interval}
			assert.Equal(t, tc.want, h.Stale(now, 2*time.Minute))
		})
	}
}

func TestHostIncompatible(t *testing.T) {
	h := &Host{}
	assert.Empty(t, h.Incompatible(), "no sync yet")
	assert.Zero(t, h.AgentProtocol())

	h.Status = &protocol.SyncRequest{ProtocolVersion: protocol.Version}
	assert.Empty(t, h.Incompatible())
	assert.Equal(t, protocol.Version, h.AgentProtocol())

	for _, v := range []int{protocol.MinVersion - 1, protocol.Version + 1} {
		h.Status.ProtocolVersion = v
		assert.Equal(t, fmt.Sprintf("agent protocol %d not supported (controller supports %d..%d)",
			v, protocol.MinVersion, protocol.Version), h.Incompatible())
	}
}
