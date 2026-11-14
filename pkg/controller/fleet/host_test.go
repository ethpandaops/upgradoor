package fleet

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	_, ok = n.EpochStart(1 << 40)
	assert.False(t, ok, "absurdly far out")
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

// TestEpochTimesBounds: a timing that is out of bounds or out of order places
// nothing from the phase on that is, everything before it as ever, and
// nothing overflows or panics. The values of the fourth review are among
// them.
func TestEpochTimesBounds(t *testing.T) {
	genesis := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	someTime := genesis.Add(1000 * time.Hour)

	type probe struct {
		epoch uint64
		at    time.Time
	}

	for _, tt := range []struct {
		name string
		n    NetworkHealth
		// valid says whether the timing itself is; within is an epoch and a
		// time that it then places, beyond ones that it does not.
		valid  bool
		within *probe
		beyond *probe
		// limit is, for a timing that is not valid, the first epoch that it
		// does not place: the epochs before the phase that is out of bounds
		// are placed by the phases before it. Zero: it places nothing. (All
		// such timings here begin at genesis with epochs of 384 s.)
		limit uint64
	}{
		{
			// 12 s * 64 slots * 2^24 epochs overflows a duration: before, the
			// start of epoch 2^24 came out before genesis, with ok.
			name:   "12 s slots, 64 per epoch, at epoch 2^24",
			n:      NetworkHealth{SlotsPerEpoch: 64, Phases: SinglePhase(genesis, 12*time.Second)},
			valid:  true,
			within: &probe{epoch: 1 << 20, at: someTime},
			beyond: &probe{epoch: 1 << 24, at: genesis.Add(maxPhaseReach + time.Hour)},
		},
		{
			// An epoch of 7.5 years: before, epoch 40 came out before genesis.
			name: "1 h slots, 65536 per epoch, at epoch 40",
			n:    NetworkHealth{SlotsPerEpoch: 65536, Phases: SinglePhase(genesis, time.Hour)},
		},
		{
			// 2^58 * 64 ms wraps to 0: before, EpochAt divided by zero.
			name: "an epoch length that wraps to zero",
			n:    NetworkHealth{SlotsPerEpoch: 1 << 58, Phases: []SlotPhase{{Start: genesis, SlotMs: 64}}},
		},
		{
			name: "a slot duration that wraps the product",
			n:    NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{{Start: genesis, SlotMs: 1 << 59}}},
		},
		{
			name: "a slot of more than an hour",
			n:    NetworkHealth{SlotsPerEpoch: 1, Phases: []SlotPhase{{Start: genesis, SlotMs: MaxSlotMs + 1}}},
		},
		{
			name: "an epoch of more than a day",
			n:    NetworkHealth{SlotsPerEpoch: 7201, Phases: SinglePhase(genesis, 12*time.Second)},
		},
		{
			name:   "an epoch of exactly a day",
			n:      NetworkHealth{SlotsPerEpoch: 7200, Phases: SinglePhase(genesis, 12*time.Second)},
			valid:  true,
			within: &probe{epoch: 36499, at: someTime},
			beyond: &probe{epoch: 36501, at: genesis.Add(maxPhaseReach + time.Nanosecond)},
		},
		{
			name: "phases out of order",
			n: NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{
				{Epoch: 100, Start: genesis.Add(100 * 384 * time.Second), SlotMs: 6000},
				{Epoch: 0, Start: genesis, SlotMs: 12000},
			}},
		},
		{
			name: "two phases at one epoch",
			n: NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{
				{Epoch: 0, Start: genesis, SlotMs: 12000},
				{Epoch: 0, Start: genesis, SlotMs: 6000},
			}},
		},
		{
			name: "a phase that does not start where the one before it ends",
			n: NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{
				{Epoch: 0, Start: genesis, SlotMs: 12000},
				{Epoch: 100, Start: genesis.Add(100*384*time.Second + time.Millisecond), SlotMs: 6000},
			}},
			limit: 100,
		},
		{
			// The finding of the fifth review: one phase out of bounds, scheduled
			// far in the future, invalidated the timing now.
			name: "a slot of two hours from epoch 1000000 on",
			n: NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{
				{Epoch: 0, Start: genesis, SlotMs: 12000},
				{Epoch: 1_000_000, Start: genesis.Add(1_000_000 * 384 * time.Second), SlotMs: 7_200_000},
			}},
			limit: 1_000_000,
		},
		{
			name: "three phases of 12 s slots, the last one a millisecond late",
			n: NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{
				{Epoch: 0, Start: genesis, SlotMs: 12000},
				{Epoch: 50, Start: genesis.Add(50 * 384 * time.Second), SlotMs: 12000},
				{Epoch: 100, Start: genesis.Add(100*384*time.Second + time.Millisecond), SlotMs: 3000},
			}},
			limit: 100,
		},
		{
			name: "a phase that starts before the one before it",
			n: NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{
				{Epoch: 0, Start: genesis, SlotMs: 12000},
				{Epoch: 100, Start: genesis.Add(-time.Hour), SlotMs: 6000},
			}},
			limit: 100,
		},
		{
			name: "a first phase too long for a duration",
			n: NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{
				{Epoch: 0, Start: genesis, SlotMs: 12000},
				{Epoch: 1 << 40, Start: genesis.Add(time.Hour), SlotMs: 6000},
			}},
			limit: 1 << 40,
		},
		{
			// A fork scheduled 121 years out is no reason to place nothing now.
			name: "a fork far in the future",
			n: NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{
				{Epoch: 0, Start: genesis, SlotMs: 12000},
				{Epoch: 10_000_000, Start: genesis.Add(10_000_000 * 384 * time.Second), SlotMs: 6000},
			}},
			valid:  true,
			within: &probe{epoch: 9375, at: someTime},
			beyond: &probe{epoch: 9_999_999, at: genesis.Add(maxPhaseReach + time.Hour)},
		},
		{
			name:   "a first phase that begins late",
			n:      NetworkHealth{SlotsPerEpoch: 32, Phases: []SlotPhase{{Epoch: 1 << 63, Start: genesis, SlotMs: 12000}}},
			valid:  true,
			within: &probe{epoch: 1<<63 + 9375, at: someTime},
			beyond: &probe{epoch: math.MaxUint64, at: genesis.Add(-time.Second)},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := tt.n
			err := ValidateTiming(n.SlotsPerEpoch, n.Phases)

			if !tt.valid {
				require.ErrorIs(t, err, ErrInvalidTiming)

				// What lies before the phase that is out of bounds, and within a
				// hundred years, is placed by the phases before it.
				placed := func(epoch uint64) bool {
					return epoch < tt.limit && epoch <= uint64(maxPhaseReach/(384*time.Second))
				}
				epochOf := func(at time.Time) uint64 { return uint64(at.Sub(genesis) / (384 * time.Second)) }

				for _, epoch := range []uint64{0, 1, 40, 99, 100, 1 << 24, math.MaxUint64} {
					n.CurrentEpoch = epoch
					start, ok := n.EpochStart(epoch)
					assert.Equal(t, placed(epoch), ok, "EpochStart(%d)", epoch)
					assert.Equal(t, placed(epoch), n.EpochDuration() == 384*time.Second, "EpochDuration at %d", epoch)
					assert.Equal(t, placed(epoch), TimingUsable(n.SlotsPerEpoch, n.Phases, epoch))

					if ok {
						assert.Equal(t, genesis.Add(time.Duration(epoch)*384*time.Second), start)
					}
				}

				for _, at := range []time.Time{genesis, someTime, genesis.Add(99 * 384 * time.Second)} {
					epoch, ok := n.EpochAt(at)
					assert.Equal(t, placed(epochOf(at)), ok, "EpochAt(%s)", at)

					if ok {
						assert.Equal(t, epochOf(at), epoch)
					}

					_, ok = n.EpochSpan(at, 1)
					assert.Equal(t, placed(epochOf(at)+1), ok, "EpochSpan(%s)", at)
				}

				for _, at := range []time.Time{genesis.Add(-time.Hour), {}} {
					_, ok := n.EpochAt(at)
					assert.False(t, ok, "EpochAt(%s)", at)

					_, ok = n.EpochSpan(at, 1)
					assert.Equal(t, placed(1), ok, "EpochSpan(%s)", at)
				}

				_, ok := n.TimingStart()
				assert.Equal(t, tt.limit > 0, ok)

				return
			}

			require.NoError(t, err)

			start, ok := n.EpochStart(tt.within.epoch)
			require.True(t, ok)
			assert.False(t, start.Before(n.Phases[0].Start), "no epoch begins before the first phase")

			at, ok := n.EpochAt(start)
			require.True(t, ok)
			assert.Equal(t, tt.within.epoch, at)

			_, ok = n.EpochAt(tt.within.at)
			assert.True(t, ok)

			_, ok = n.EpochSpan(tt.within.at, 2)
			assert.True(t, ok)

			_, ok = n.EpochStart(tt.beyond.epoch)
			assert.False(t, ok, "EpochStart beyond")

			_, ok = n.EpochAt(tt.beyond.at)
			assert.False(t, ok, "EpochAt beyond")

			_, ok = n.EpochSpan(tt.within.at, math.MaxUint64)
			assert.False(t, ok, "a span that wraps the epoch number")

			n.CurrentEpoch = math.MaxUint64
			assert.Zero(t, n.EpochDuration())
		})
	}

	assert.ErrorIs(t, ValidateTiming(0, SinglePhase(genesis, 12*time.Second)), ErrNoTiming)
	assert.ErrorIs(t, ValidateTiming(32, nil), ErrNoTiming)
	assert.ErrorIs(t, ValidateTiming(MaxSlotsPerEpoch+1, SinglePhase(genesis, time.Millisecond)), ErrInvalidTiming)
}

// randomTiming builds a valid timing of one to four phases from a random
// source: slots of 1 ms to an hour, epochs of at most a day.
func randomTiming(rng *rand.Rand) *NetworkHealth {
	genesis := time.Unix(int64(rng.Uint32()), int64(rng.IntN(1000))*int64(time.Millisecond)).UTC()

	slots := uint64(1 + rng.IntN(64))
	if rng.IntN(8) == 0 {
		slots = uint64(1 + rng.IntN(MaxSlotsPerEpoch))
	}

	slotMs := func() uint64 {
		longest := min(uint64(MaxSlotMs), uint64(MaxEpochLength/time.Millisecond)/slots)

		switch rng.IntN(4) {
		case 0:
			return 1 + rng.Uint64N(longest)
		case 1:
			return longest
		default:
			return 1 + rng.Uint64N(min(longest, 12_000))
		}
	}

	n := &NetworkHealth{
		SlotsPerEpoch: slots,
		Phases:        []SlotPhase{{Epoch: uint64(rng.IntN(3)) * rng.Uint64N(1000), Start: genesis, SlotMs: slotMs()}},
	}

	for range rng.IntN(4) {
		last := n.Phases[len(n.Phases)-1]
		length := time.Duration(last.SlotMs*slots) * time.Millisecond
		epochs := 1 + rng.Uint64N(5000)

		n.Phases = append(n.Phases, SlotPhase{
			Epoch:  last.Epoch + epochs,
			Start:  last.Start.Add(time.Duration(epochs) * length),
			SlotMs: slotMs(),
		})
	}

	return n
}

// checkInvalidTiming holds a timing that is not valid against its longest
// valid beginning: what it places, it places as that does, and nothing in or
// after the first phase that is out of bounds.
func checkInvalidTiming(n *NetworkHealth, epoch uint64, invalid error) error {
	valid := 0
	for valid < len(n.Phases) && ValidateTiming(n.SlotsPerEpoch, n.Phases[:valid+1]) == nil {
		valid++
	}

	at := time.Unix(int64(epoch%(1<<32)), 0)
	before := &NetworkHealth{SlotsPerEpoch: n.SlotsPerEpoch, Phases: n.Phases[:valid]}

	// The first epoch that is not placed: where the phase begins that is out
	// of bounds or does not follow from the one before it.
	limit := uint64(0)
	if valid > 0 && valid < len(n.Phases) {
		limit = n.Phases[valid].Epoch
	}

	start, ok := n.EpochStart(epoch)
	want, wantOK := before.EpochStart(epoch)

	switch {
	case ok && (valid == 0 || epoch >= limit):
		return fmt.Errorf("EpochStart(%d) is placed in or after a phase that is invalid: %w", epoch, invalid)
	case ok != (wantOK && epoch < limit), ok && !start.Equal(want):
		return fmt.Errorf("EpochStart(%d) = %s, %v; the valid phases before give %s, %v (limit %d)",
			epoch, start, ok, want, wantOK, limit)
	}

	got, ok := n.EpochAt(at)
	wantAt, wantOK := before.EpochAt(at)

	switch {
	case ok && (valid == 0 || got >= limit):
		return fmt.Errorf("EpochAt is placed in or after a phase that is invalid: %w", invalid)
	case ok != (wantOK && wantAt < limit), ok && got != wantAt:
		return fmt.Errorf("EpochAt(%s) = %d, %v; the valid phases before give %d, %v (limit %d)",
			at, got, ok, wantAt, wantOK, limit)
	}

	span, ok := n.EpochSpan(at, 1)
	wantSpan, wantOK := before.EpochSpan(at, 1)

	switch {
	case ok && valid == 0:
		return fmt.Errorf("EpochSpan is placed by a timing that is invalid from its first phase on: %w", invalid)
	case ok && (!wantOK || span != wantSpan):
		return fmt.Errorf("EpochSpan(%s) = %s; the valid phases before give %s, %v", at, span, wantSpan, wantOK)
	}

	if _, placed := n.TimingStart(); placed != (valid > 0 && limit > n.Phases[0].Epoch) {
		return fmt.Errorf("TimingStart is placed: %v, with %d valid phases and the limit %d", placed, valid, limit)
	}

	return nil
}

// checkTiming holds the helpers of a timing against each other at one epoch
// and one offset into it. It returns the first disagreement.
func checkTiming(n *NetworkHealth, epoch uint64, offset time.Duration) error {
	if err := ValidateTiming(n.SlotsPerEpoch, n.Phases); err != nil {
		return checkInvalidTiming(n, epoch, err)
	}

	start, ok := n.EpochStart(epoch)
	if !ok {
		return nil
	}

	if start.Before(n.Phases[0].Start) {
		return fmt.Errorf("epoch %d starts at %s, before the first phase", epoch, start)
	}

	at, ok := n.EpochAt(start)
	if !ok || at != epoch {
		return fmt.Errorf("EpochAt(EpochStart(%d)) = %d, %v", epoch, at, ok)
	}

	if epoch == math.MaxUint64 {
		return nil
	}

	next, ok := n.EpochStart(epoch + 1)
	if !ok {
		return nil
	}

	length := next.Sub(start)
	if length <= 0 || length > MaxEpochLength {
		return fmt.Errorf("epoch %d lasts %s", epoch, length)
	}

	// Every time inside the epoch is in it, and the last nanosecond before it
	// is in the epoch before.
	inside := start.Add(offset % length)
	if at, ok = n.EpochAt(inside); !ok || at != epoch {
		return fmt.Errorf("EpochAt(%s) = %d, %v, inside epoch %d", inside, at, ok, epoch)
	}

	if at, ok = n.EpochAt(next.Add(-time.Nanosecond)); !ok || at != epoch {
		return fmt.Errorf("the last nanosecond of epoch %d is in epoch %d, %v", epoch, at, ok)
	}

	// The span of the epochs that begin with this one is the sum of their
	// lengths.
	if span, placed := n.EpochSpan(inside, 1); !placed || span != length {
		return fmt.Errorf("EpochSpan of epoch %d is %s, %v, the epoch lasts %s", epoch, span, placed, length)
	}

	if after, placed := n.EpochStart(epoch + 2); placed {
		if span, spanned := n.EpochSpan(inside, 2); !spanned || span != after.Sub(start) {
			return fmt.Errorf("EpochSpan of two epochs from %d is %s, %v, they last %s",
				epoch, span, spanned, after.Sub(start))
		}
	}

	return nil
}

// TestEpochTimesProperties: for random valid timings, the epoch at the start
// of an epoch is that epoch, epochs follow each other without a gap, and
// their starts and the epochs of ascending times are ascending.
func TestEpochTimesProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 2026))

	for range 2000 {
		n := randomTiming(rng)
		require.NoError(t, ValidateTiming(n.SlotsPerEpoch, n.Phases))

		first := n.Phases[0].Epoch
		last := n.Phases[len(n.Phases)-1].Epoch

		// Epochs around every phase boundary and random ones up to a little
		// past the last phase.
		epochs := make([]uint64, 0, 3*len(n.Phases)+8)
		epochs = append(epochs, first, first+1)

		for _, phase := range n.Phases[1:] {
			epochs = append(epochs, phase.Epoch-1, phase.Epoch, phase.Epoch+1)
		}

		for range 8 {
			epochs = append(epochs, first+rng.Uint64N(last-first+2000))
		}

		slices.Sort(epochs)

		var (
			before  time.Time
			started bool
		)

		for i, epoch := range epochs {
			offset := time.Duration(rng.Int64N(int64(MaxEpochLength)))
			require.NoError(t, checkTiming(n, epoch, offset), "timing %+v", n)

			start, ok := n.EpochStart(epoch)
			require.True(t, ok, "epoch %d of %+v", epoch, n)

			if started && epochs[i-1] != epoch {
				require.True(t, start.After(before), "epoch %d does not start after epoch %d in %+v",
					epoch, epochs[i-1], n)
			}

			before, started = start, true
		}

		// Ascending times lie in ascending epochs.
		reach := before.Sub(n.Phases[0].Start) + time.Hour
		times := make([]time.Duration, 0, 16)

		for range 16 {
			times = append(times, time.Duration(rng.Int64N(int64(reach))))
		}

		slices.Sort(times)

		var previous uint64

		for i, d := range times {
			at, ok := n.EpochAt(n.Phases[0].Start.Add(d))
			require.True(t, ok)

			if i > 0 {
				require.GreaterOrEqual(t, at, previous, "time %s of %+v", d, n)
			}

			previous = at
		}
	}
}

// FuzzEpochTimes feeds the helpers arbitrary timings, valid or not: they
// never panic, an invalid timing places nothing in or after its first phase
// that is out of bounds and everything before it as its valid beginning
// does, and what a valid one places agrees with itself.
func FuzzEpochTimes(f *testing.F) {
	f.Add(uint64(32), uint64(12000), uint64(0), uint64(100), uint64(6000), int64(0), uint64(150), int64(1000))
	f.Add(uint64(64), uint64(12000), uint64(0), uint64(0), uint64(0), int64(0), uint64(1<<24), int64(0))
	f.Add(uint64(65536), uint64(3_600_000), uint64(0), uint64(0), uint64(0), int64(0), uint64(40), int64(0))
	f.Add(uint64(1<<58), uint64(64), uint64(0), uint64(0), uint64(0), int64(0), uint64(1), int64(5))
	f.Add(uint64(32), uint64(12000), uint64(100), uint64(0), uint64(6000), int64(0), uint64(50), int64(7))
	f.Add(uint64(32), uint64(4500), uint64(5), uint64(200), uint64(12000), int64(1), uint64(250), int64(-3))
	f.Add(uint64(1), uint64(1), uint64(math.MaxUint64-1), uint64(1), uint64(1), int64(0), uint64(math.MaxUint64), int64(0))

	genesis := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	f.Fuzz(func(t *testing.T, slots, slotMs, firstEpoch, forkAfter, forkSlotMs uint64, skew int64, epoch uint64, offset int64) {
		n := &NetworkHealth{
			SlotsPerEpoch: slots,
			Phases:        []SlotPhase{{Epoch: firstEpoch, Start: genesis, SlotMs: slotMs}},
		}

		// A second phase forkAfter epochs later, aligned unless skew is set.
		if forkSlotMs != 0 {
			length, ok := epochLength(slots, slotMs)
			start := genesis.Add(time.Duration(skew))

			if ok && forkAfter <= uint64(math.MaxInt64/length) {
				start = start.Add(time.Duration(forkAfter) * length)
			}

			n.Phases = append(n.Phases, SlotPhase{Epoch: firstEpoch + forkAfter, Start: start, SlotMs: forkSlotMs})
		}

		if offset < 0 {
			offset = -(offset + 1)
		}

		if err := checkTiming(n, epoch, time.Duration(offset)); err != nil {
			t.Fatalf("%v\ntiming: %+v", err, n)
		}

		// Whatever the timing, nothing panics on the edges.
		n.CurrentEpoch = epoch
		_ = n.EpochDuration()
		_, _ = n.EpochAt(time.Time{})
		_, _ = n.EpochAt(genesis.Add(time.Duration(math.MaxInt64)))
		_, _ = n.EpochSpan(genesis.Add(time.Duration(offset)), epoch)
		_, _ = n.EpochStart(math.MaxUint64)
	})
}
