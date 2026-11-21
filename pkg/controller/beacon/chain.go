package beacon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

const (
	specSecondsPerSlot   = "SECONDS_PER_SLOT"
	specSlotDurationMS   = "SLOT_DURATION_MS"
	specSlotsPerEpoch    = "SLOTS_PER_EPOCH"
	specForkEpochSuffix  = "_FORK_EPOCH"
	specSlotDurationFork = specSlotDurationMS + "_"
)

// slotPhase is a stretch of the chain with one slot duration. A fork that
// changes the slot duration (SLOT_DURATION_MS_<FORK>) starts a new phase at
// its fork epoch.
type slotPhase struct {
	// epoch is the first epoch of the phase, start its wall-clock start.
	epoch uint64
	start time.Time
	slot  time.Duration
}

// chainTiming is the timing read from the node. Between the wall clock and
// slots / epochs it converts through clock, the fleet.NetworkHealth methods
// the scheduler and the API use as well.
type chainTiming struct {
	genesis       time.Time
	slotsPerEpoch uint64
	// phases is ordered by epoch; phases[0].epoch is always 0.
	phases []slotPhase
	clock  fleet.NetworkHealth
}

// slotChange is a slot duration that takes effect at an epoch.
type slotChange struct {
	epoch uint64
	key   string
	slot  time.Duration
}

// newChainTiming builds the timing from the genesis time and the node's
// /eth/v1/config/spec values. SLOT_DURATION_MS wins over SECONDS_PER_SLOT;
// SLOT_DURATION_MS_<FORK> applies from <FORK>_FORK_EPOCH on.
func newChainTiming(genesis time.Time, spec map[string]json.RawMessage) (*chainTiming, error) {
	slotsPerEpoch, ok, err := specUint(spec, specSlotsPerEpoch)
	if err != nil {
		return nil, err
	}

	if !ok || slotsPerEpoch == 0 {
		return nil, fmt.Errorf("beacon spec has no usable %s", specSlotsPerEpoch)
	}

	base, err := baseSlotDuration(spec)
	if err != nil {
		return nil, err
	}

	changes, err := forkSlotChanges(spec)
	if err != nil {
		return nil, err
	}

	timing := &chainTiming{
		genesis:       genesis,
		slotsPerEpoch: slotsPerEpoch,
		phases:        make([]slotPhase, 0, len(changes)+1),
	}
	timing.phases = append(timing.phases, slotPhase{epoch: 0, start: genesis, slot: base})

	for _, change := range changes {
		last := &timing.phases[len(timing.phases)-1]
		if change.epoch == last.epoch {
			last.slot = change.slot

			continue
		}

		span, fits := spanDuration(change.epoch-last.epoch, slotsPerEpoch, last.slot)
		if !fits {
			// Scheduled further out than the clock can express: not relevant.
			break
		}

		timing.phases = append(timing.phases, slotPhase{
			epoch: change.epoch,
			start: last.start.Add(span),
			slot:  change.slot,
		})
	}

	timing.clock = fleet.NetworkHealth{SlotsPerEpoch: slotsPerEpoch, Phases: timing.published()}

	return timing, nil
}

// baseSlotDuration returns the slot duration at genesis.
func baseSlotDuration(spec map[string]json.RawMessage) (time.Duration, error) {
	ms, ok, err := specUint(spec, specSlotDurationMS)
	if err != nil {
		return 0, err
	}

	if !ok || ms == 0 {
		seconds, found, secErr := specUint(spec, specSecondsPerSlot)
		if secErr != nil {
			return 0, secErr
		}

		if !found || seconds == 0 || seconds > math.MaxInt64/1000 {
			return 0, fmt.Errorf("beacon spec has no usable %s or %s",
				specSlotDurationMS, specSecondsPerSlot)
		}

		ms = seconds * 1000
	}

	return millis(ms)
}

// forkSlotChanges collects SLOT_DURATION_MS_<FORK> values of scheduled
// forks, ordered by fork epoch.
func forkSlotChanges(spec map[string]json.RawMessage) ([]slotChange, error) {
	changes := make([]slotChange, 0, 2)

	for key := range spec {
		fork, isFork := strings.CutPrefix(key, specSlotDurationFork)
		if !isFork || fork == "" {
			continue
		}

		epoch, scheduled, err := specUint(spec, fork+specForkEpochSuffix)
		if err != nil {
			return nil, err
		}

		if !scheduled || epoch == math.MaxUint64 {
			continue
		}

		ms, _, err := specUint(spec, key)
		if err != nil {
			return nil, err
		}

		if ms == 0 {
			continue
		}

		slot, err := millis(ms)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}

		changes = append(changes, slotChange{epoch: epoch, key: key, slot: slot})
	}

	sort.Slice(changes, func(i, j int) bool {
		if changes[i].epoch != changes[j].epoch {
			return changes[i].epoch < changes[j].epoch
		}

		return changes[i].key < changes[j].key
	})

	return changes, nil
}

// millis converts a millisecond count to a duration.
func millis(ms uint64) (time.Duration, error) {
	if ms > uint64(math.MaxInt64/int64(time.Millisecond)) {
		return 0, errors.New("slot duration out of range")
	}

	return time.Duration(ms) * time.Millisecond, nil
}

// spanDuration returns epochs*slotsPerEpoch*slot, or false on overflow.
func spanDuration(epochs, slotsPerEpoch uint64, slot time.Duration) (time.Duration, bool) {
	if epochs > math.MaxUint64/slotsPerEpoch {
		return 0, false
	}

	slots := epochs * slotsPerEpoch
	if slots > uint64(math.MaxInt64/int64(slot)) {
		return 0, false
	}

	return time.Duration(slots) * slot, true
}

// specUint reads an unsigned integer spec value. Beacon nodes encode them as
// decimal strings; a bare JSON number is accepted too. Values of another
// shape (lists, hex strings) report an error only when asked for.
func specUint(spec map[string]json.RawMessage, key string) (value uint64, ok bool, err error) {
	raw, present := spec[key]
	if !present {
		return 0, false, nil
	}

	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)

	value, err = strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("beacon spec value %s is not an unsigned integer", key)
	}

	return value, true, nil
}

// slotAt returns the slot the wall clock is in; 0 before genesis or while
// the timing is invalid (fleet.ValidateTiming).
func (c *chainTiming) slotAt(t time.Time) uint64 {
	slot, _ := c.clock.SlotAt(t)

	return slot
}

// epochAt returns the epoch the wall clock is in; 0 as for slotAt.
func (c *chainTiming) epochAt(t time.Time) uint64 {
	return c.slotAt(t) / c.slotsPerEpoch
}

// slotDuration returns the slot duration in effect during epoch.
func (c *chainTiming) slotDuration(epoch uint64) time.Duration {
	phase := c.phases[0]

	for _, next := range c.phases[1:] {
		if epoch < next.epoch {
			break
		}

		phase = next
	}

	return phase.slot
}

// wholeSeconds reports a slot duration in whole seconds (rounded, at least 1)
// for consumers that only know seconds.
func wholeSeconds(slot time.Duration) uint64 {
	seconds := uint64((slot + time.Second/2) / time.Second)
	if seconds == 0 {
		return 1
	}

	return seconds
}

// published returns the phases as fleet.NetworkHealth carries them.
func (c *chainTiming) published() []fleet.SlotPhase {
	out := make([]fleet.SlotPhase, 0, len(c.phases))

	for _, phase := range c.phases {
		out = append(out, fleet.SlotPhase{
			Epoch:  phase.epoch,
			Start:  phase.start,
			SlotMs: uint64(phase.slot / time.Millisecond),
		})
	}

	return out
}
