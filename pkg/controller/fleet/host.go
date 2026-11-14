// Package fleet is the controller's domain model: hosts, needs, rollouts,
// leases, settings, the audit trail and network health. It holds types and
// small pure helpers only; persistence lives in store, decisions in
// scheduler.
package fleet

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// RoleBootnode is the certificate role that marks a bootnode. It is shown
// with the host; the scheduler treats a bootnode like any other host.
const RoleBootnode = "bootnode"

// Host is one host known to the controller. A host exists once a valid
// certificate has talked to the controller.
type Host struct {
	Name string `json:"name"`
	// CertHash is the hash of the certificate last presented. A change is
	// audited, not rejected.
	CertHash string `json:"certHash"`
	// Generation, Validators and Roles are copied from the certificate.
	Generation uint64                   `json:"generation"`
	Validators *identity.ValidatorRange `json:"validators,omitempty"`
	Roles      []string                 `json:"roles"`

	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	// RemoteAddr is the address the last sync came from.
	RemoteAddr string `json:"remoteAddr,omitempty"`

	// Status is the last sync request, verbatim.
	Status *protocol.SyncRequest `json:"status,omitempty"`
	// InventoryHash is the hash of the inventory stored for this host.
	InventoryHash string `json:"inventoryHash,omitempty"`

	// HoldWanted is the controller-side hold switch; the scheduler turns it
	// into hold / release-hold directives until the agent agrees.
	HoldWanted bool   `json:"holdWanted"`
	HoldReason string `json:"holdReason,omitempty"`
}

// Weight is the number of validators the host operates.
func (h *Host) Weight() uint64 { return h.Validators.Count() }

// Stale reports whether the host's agent has been silent for longer than
// staleAfter.
func (h *Host) Stale(now time.Time, staleAfter time.Duration) bool {
	return now.Sub(h.LastSeen) > staleAfter
}

// Held reports whether the agent reports any hold or a controller hold is
// wanted.
func (h *Host) Held() bool {
	return h.HoldWanted || (h.Status != nil && h.Status.Hold.Active())
}

// Liveness is the attestation liveness of one host's validators in one epoch.
type Liveness struct {
	Host  string `json:"host"`
	Epoch uint64 `json:"epoch"`
	Live  uint64 `json:"live"`
	Total uint64 `json:"total"`
}

// NetworkHealth is the beacon service's view of the network. The scheduler
// evaluates the network gate from it.
type NetworkHealth struct {
	// UpdatedAt is when the view was last refreshed successfully; Error is
	// the last refresh error, if the latest attempt failed.
	UpdatedAt time.Time `json:"updatedAt"`
	Error     string    `json:"error,omitempty"`

	GenesisTime time.Time `json:"genesisTime"`
	// SecondsPerSlot is the current slot duration rounded down to whole
	// seconds, for display. Nothing is computed from it: see Phases.
	SecondsPerSlot uint64 `json:"secondsPerSlot"`
	SlotsPerEpoch  uint64 `json:"slotsPerEpoch"`
	// Phases are the stretches of the chain with one slot duration each,
	// ordered by epoch, the first one starting at genesis. A fork that
	// changes the slot duration starts a new one. Every conversion between
	// an epoch and a time goes through them (EpochStart, EpochAt, EpochSpan);
	// while they are empty no epoch can be placed in time, and none in or
	// after a phase that is invalid (ValidateTiming).
	Phases []SlotPhase `json:"phases,omitempty"`
	// TimingError says why the current epoch cannot be placed by Phases: the
	// phase it lies in, or one before it, is out of bounds. Empty while the
	// timing is good for now (a phase that is out of bounds further on is
	// logged by the beacon service and closes nothing yet). While it is set
	// the network gate is closed with it as the reason.
	TimingError string `json:"timingError,omitempty"`

	HeadSlot       uint64 `json:"headSlot"`
	CurrentEpoch   uint64 `json:"currentEpoch"`
	FinalizedEpoch uint64 `json:"finalizedEpoch"`
	// FinalityLagEpochs is CurrentEpoch - FinalizedEpoch. A healthy network
	// sits at 2.
	FinalityLagEpochs uint64 `json:"finalityLagEpochs"`

	// LivenessEpoch is the newest epoch with stored liveness; LivenessKnown
	// is false until the first one is stored or when the beacon node does not
	// serve the liveness endpoint.
	LivenessKnown bool   `json:"livenessKnown"`
	LivenessEpoch uint64 `json:"livenessEpoch"`
	// LivenessUnsupported is true while the beacon node is known not to serve
	// the liveness endpoint (as opposed to liveness that is merely not there
	// yet or failed to refresh). The scheduler then judges availability and
	// readiness from the agents' probes and the gate from finality alone.
	LivenessUnsupported bool `json:"livenessUnsupported"`
	// Tracked and Live count the validators of all hosts in LivenessEpoch.
	Tracked uint64 `json:"tracked"`
	Live    uint64 `json:"live"`
}

// SlotPhase is a stretch of the chain with one slot duration.
type SlotPhase struct {
	// Epoch is the first epoch of the phase, Start its wall-clock start.
	Epoch uint64    `json:"epoch"`
	Start time.Time `json:"start"`
	// SlotMs is the slot duration in milliseconds.
	SlotMs uint64 `json:"slotMs"`
}

// SinglePhase is the timing of a chain whose slot duration never changes.
func SinglePhase(genesis time.Time, slot time.Duration) []SlotPhase {
	return []SlotPhase{{Epoch: 0, Start: genesis, SlotMs: uint64(slot / time.Millisecond)}}
}

// Bounds of a chain timing the controller works with. A slot phase outside
// them is broken, and no epoch is placed by it or after it: every check that
// needs the time of such an epoch fails safe. The epochs before it are placed
// as if it were not there.
const (
	// MaxSlotMs is the longest slot: one hour.
	MaxSlotMs = 3_600_000
	// MaxSlotsPerEpoch is the largest number of slots in an epoch.
	MaxSlotsPerEpoch = 1 << 16
	// MaxEpochLength is the longest epoch: one day.
	MaxEpochLength = 24 * time.Hour
	// maxPhaseReach is how far past its start a phase is extrapolated: a
	// hundred years. It keeps every time and duration far inside what
	// time.Duration can hold.
	maxPhaseReach = 100 * 365 * 24 * time.Hour
)

// Errors of ValidateTiming.
var (
	ErrNoTiming      = errors.New("no chain timing known")
	ErrInvalidTiming = errors.New("invalid chain timing")
)

// ValidateTiming checks the slot phases of a chain: there is at least one,
// every slot lasts between 1 ms and MaxSlotMs, an epoch has between 1 and
// MaxSlotsPerEpoch slots and lasts at most MaxEpochLength, the phases are in
// ascending order of epoch, and every phase starts exactly where the one
// before it ends. The error names the first phase that fails this. Such a
// timing is still good for the epochs before that phase: see TimingUsable.
func ValidateTiming(slotsPerEpoch uint64, phases []SlotPhase) error {
	_, _, err := usableTiming(slotsPerEpoch, phases)

	return err
}

// TimingUsable reports whether a timing places the epoch: its phases are
// valid at least up to and including the phase the epoch lies in. A phase
// that is out of bounds further on, a fork scheduled for the future,
// invalidates nothing before it.
func TimingUsable(slotsPerEpoch uint64, phases []SlotPhase, epoch uint64) bool {
	n := NetworkHealth{SlotsPerEpoch: slotsPerEpoch, Phases: phases}
	_, ok := n.EpochStart(epoch)

	return ok
}

// usableTiming validates the phases in order. usable is how many leading
// phases place epochs; limit is the first epoch that none of them places
// (the first epoch of the phase that is invalid, or that does not follow
// from the one before it), and is meaningful only when err is not nil. With
// a nil error every phase is usable and there is no limit.
func usableTiming(slotsPerEpoch uint64, phases []SlotPhase) (usable int, limit uint64, err error) {
	if slotsPerEpoch == 0 || len(phases) == 0 {
		return 0, 0, ErrNoTiming
	}

	if slotsPerEpoch > MaxSlotsPerEpoch {
		return 0, 0, fmt.Errorf("%w: %d slots per epoch, at most %d", ErrInvalidTiming,
			slotsPerEpoch, uint64(MaxSlotsPerEpoch))
	}

	for i := range phases {
		phase := &phases[i]

		length, ok := epochLength(slotsPerEpoch, phase.SlotMs)
		if !ok {
			return i, phase.Epoch, fmt.Errorf("%w: phase %d (from epoch %d) has slots of %d ms and epochs of "+
				"%d slots, a slot must last 1 to %d ms and an epoch at most %s", ErrInvalidTiming,
				i, phase.Epoch, phase.SlotMs, slotsPerEpoch, MaxSlotMs, MaxEpochLength)
		}

		if i+1 == len(phases) {
			break
		}

		next := &phases[i+1]
		if next.Epoch <= phase.Epoch {
			return i + 1, next.Epoch, fmt.Errorf("%w: phase %d begins at epoch %d, not after epoch %d of the "+
				"phase before it", ErrInvalidTiming, i+1, next.Epoch, phase.Epoch)
		}

		// The whole length of a phase must fit a duration; how far the last
		// one is extrapolated is bounded by maxPhaseReach.
		epochs := next.Epoch - phase.Epoch
		if epochs > uint64(math.MaxInt64/length) {
			return i + 1, next.Epoch, fmt.Errorf("%w: phase %d (from epoch %d) is too long to place the next one",
				ErrInvalidTiming, i, phase.Epoch)
		}

		if end := phase.Start.Add(time.Duration(epochs) * length); !next.Start.Equal(end) {
			return i + 1, next.Epoch, fmt.Errorf("%w: phase %d (from epoch %d) starts at %s, the phase before "+
				"it ends at %s", ErrInvalidTiming, i+1, next.Epoch, next.Start.UTC().Format(time.RFC3339Nano),
				end.UTC().Format(time.RFC3339Nano))
		}
	}

	return len(phases), 0, nil
}

// epochLength is the length of an epoch of slotsPerEpoch slots of slotMs
// milliseconds; ok is false when it is outside the bounds.
func epochLength(slotsPerEpoch, slotMs uint64) (length time.Duration, ok bool) {
	if slotsPerEpoch == 0 || slotsPerEpoch > MaxSlotsPerEpoch || slotMs == 0 || slotMs > MaxSlotMs {
		return 0, false
	}

	// At most 2^16 * 3.6e6 ms: no overflow.
	length = time.Duration(slotsPerEpoch*slotMs) * time.Millisecond

	return length, length <= MaxEpochLength
}

// timing is the part of a chain's slot phases that places epochs.
type timing struct {
	slotsPerEpoch uint64
	// phases are the valid leading phases; never empty.
	phases []SlotPhase
	// limited says that a phase after them is invalid; limit is then the
	// first epoch that cannot be placed.
	limited bool
	limit   uint64
}

// timing returns the usable part of the view's timing. ok is false when no
// epoch at all can be placed: the timing is unknown, or invalid from its
// first phase on.
func (n *NetworkHealth) timing() (t timing, ok bool) {
	usable, limit, err := usableTiming(n.SlotsPerEpoch, n.Phases)
	if usable == 0 || (err != nil && limit <= n.Phases[0].Epoch) {
		return timing{}, false
	}

	return timing{
		slotsPerEpoch: n.SlotsPerEpoch,
		phases:        n.Phases[:usable],
		limited:       err != nil,
		limit:         limit,
	}, true
}

// places reports whether the epoch lies inside what the timing covers.
func (t *timing) places(epoch uint64) bool {
	return epoch >= t.phases[0].Epoch && (!t.limited || epoch < t.limit)
}

// phaseOf returns the phase the epoch lies in and the length of its epochs.
// The caller has checked places.
func (t *timing) phaseOf(epoch uint64) (phase *SlotPhase, length time.Duration) {
	phase = &t.phases[0]

	for i := 1; i < len(t.phases) && t.phases[i].Epoch <= epoch; i++ {
		phase = &t.phases[i]
	}

	// Valid: checked by usableTiming.
	length, _ = epochLength(t.slotsPerEpoch, phase.SlotMs)

	return phase, length
}

// TimingStart returns the start of the first known phase: the earliest time
// that lies in an epoch. On a chain whose timing is known from genesis on it
// is the genesis time. ok is false when the timing is unknown or invalid
// from its first phase on.
func (n *NetworkHealth) TimingStart() (start time.Time, ok bool) {
	t, ok := n.timing()
	if !ok {
		return time.Time{}, false
	}

	return t.phases[0].Start, true
}

// EpochStart returns the wall-clock start of an epoch. ok is false when it
// cannot be placed: the timing is unknown, the epoch lies before the first
// known phase, in or after a phase that is invalid (ValidateTiming), or more
// than a hundred years after the start of its phase.
func (n *NetworkHealth) EpochStart(epoch uint64) (start time.Time, ok bool) {
	t, ok := n.timing()
	if !ok || !t.places(epoch) {
		return time.Time{}, false
	}

	phase, length := t.phaseOf(epoch)

	epochs := epoch - phase.Epoch
	if epochs > uint64(maxPhaseReach/length) {
		return time.Time{}, false
	}

	return phase.Start.Add(time.Duration(epochs) * length), true
}

// EpochAt returns the epoch that contains t. ok is false when it cannot be
// told: the timing is unknown, t lies before the first known phase, in or
// after a phase that is invalid (ValidateTiming), or more than a hundred
// years after the start of its phase.
func (n *NetworkHealth) EpochAt(at time.Time) (epoch uint64, ok bool) {
	t, ok := n.timing()
	if !ok || at.Before(t.phases[0].Start) {
		return 0, false
	}

	phase := &t.phases[0]

	for i := 1; i < len(t.phases) && !at.Before(t.phases[i].Start); i++ {
		phase = &t.phases[i]
	}

	// Valid: checked by usableTiming. Sub saturates far beyond maxPhaseReach.
	length, _ := epochLength(t.slotsPerEpoch, phase.SlotMs)

	since := at.Sub(phase.Start)
	if since > maxPhaseReach {
		return 0, false
	}

	epochs := uint64(since / length)
	if epochs > math.MaxUint64-phase.Epoch {
		return 0, false
	}

	// Counted on from the last valid phase, the time may lie where an invalid
	// one begins.
	if epoch = phase.Epoch + epochs; !t.places(epoch) {
		return 0, false
	}

	return epoch, true
}

// EpochSpan returns how long the count epochs take that begin with the
// epoch containing from: the time from the start of that epoch to the start
// of the epoch count later. It is exact across a change of the slot
// duration. A from before the first known phase (before genesis) counts as
// the start of that phase: no epoch begins earlier. ok is false when the
// epochs cannot be placed.
func (n *NetworkHealth) EpochSpan(from time.Time, count uint64) (span time.Duration, ok bool) {
	first, ok := n.TimingStart()
	if !ok {
		return 0, false
	}

	if from.Before(first) {
		from = first
	}

	epoch, ok := n.EpochAt(from)
	if !ok || count > math.MaxUint64-epoch {
		return 0, false
	}

	start, ok := n.EpochStart(epoch)
	if !ok {
		return 0, false
	}

	end, ok := n.EpochStart(epoch + count)
	if !ok {
		return 0, false
	}

	return end.Sub(start), true
}

// EpochDuration returns the length of the current epoch, or 0 when it cannot
// be placed. It is for display and for rough bounds; the time of a particular
// epoch comes from EpochStart.
func (n *NetworkHealth) EpochDuration() time.Duration {
	t, ok := n.timing()
	if !ok || !t.places(n.CurrentEpoch) {
		return 0
	}

	if _, placed := n.EpochStart(n.CurrentEpoch); !placed {
		return 0
	}

	_, length := t.phaseOf(n.CurrentEpoch)

	return length
}

// Participation is Live/Tracked, or 1 when nothing is tracked.
func (n *NetworkHealth) Participation() float64 {
	if n.Tracked == 0 {
		return 1
	}

	return float64(n.Live) / float64(n.Tracked)
}
