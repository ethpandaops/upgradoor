// Package fleet is the controller's domain model: hosts, needs, rollouts,
// leases, settings, the audit trail and network health. It holds types and
// small pure helpers only; persistence lives in store, decisions in
// scheduler.
package fleet

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Host is one host known to the controller. A host exists once a valid
// certificate has talked to the controller.
type Host struct {
	Name string `json:"name"`
	// CertHash is the hash of the certificate last presented. A change is
	// audited, not rejected.
	CertHash string `json:"certHash"`
	// Validators are the validator index ranges the validator ranges
	// inventory assigns to the host by name, ordered by start; nil when it
	// assigns none. They are the host's weight and the validators whose
	// attestations are tracked for it.
	Validators []identity.ValidatorRange `json:"validators,omitempty"`
	// Roles are copied from the certificate.
	Roles []string `json:"roles"`

	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	// RemoteAddr is the address the last sync came from.
	RemoteAddr string `json:"remoteAddr,omitempty"`

	// Status is the last sync request, verbatim.
	Status *protocol.SyncRequest `json:"status,omitempty"`
	// StatusHash identifies Status without what changes on every sync
	// (SentAt, UptimeSeconds, the probes' heads and peer counts); empty when
	// the store keeps none.
	StatusHash string `json:"-"`
	// InventoryHash is the hash of the inventory stored for this host.
	InventoryHash string `json:"inventoryHash,omitempty"`

	// Offline is a person's mark that the host is known offline, for example
	// a client kept broken on purpose for debugging; nil while unmarked. While
	// it is in force (KnownOffline) the host's validators are left out of the
	// gate's participation and the budget, and the host gets no lease.
	Offline *OfflineMark `json:"offline,omitempty"`

	// Broken says what the host's last lease failed on: per group of rollouts
	// (a unit as "unit:<kind>#<team>", or "packages") the ID of the
	// lease that failed. A host that is broken for the group of a rollout goes
	// first in it. The scheduler sets the mark where a lease fails and clears
	// it where a lease is released with the group's steps done; it goes with
	// the host when the host is forgotten.
	Broken map[string]string `json:"broken,omitempty"`

	// Unproven is when a lease of the host failed after it may have disrupted
	// the host; nil when no such lease is waiting for proof that the host is
	// back. While it is set the host counts as down for the budget, the host
	// limit and the gate. The scheduler sets it where such a lease fails and
	// clears it when the proof arrives (a liveness epoch, or for a host
	// without validators a healthy sync, after it), when the host goes stale,
	// or when a lease of it is released through the full readiness check.
	Unproven *time.Time `json:"unproven,omitempty"`

	// SyncInterval is the longest sync interval the controller asked of the
	// host in its last answer. StaleAfter allows for it.
	SyncInterval time.Duration `json:"syncInterval,omitempty"`
}

// HolderStaff is the holder of the hold set through the plain hold feature.
// Every other holder a person sets is a team.
const HolderStaff = protocol.HolderStaff

// Holds are the holds the host's agent reports, in protocol.HoldOrder.
func (h *Host) Holds() []protocol.HostHold {
	if h.Status == nil {
		return nil
	}

	return h.Status.Hold.Holds
}

// HoldOf returns the reported hold of holder, or nil.
func (h *Host) HoldOf(holder string) *protocol.HostHold {
	if h.Status == nil {
		return nil
	}

	return h.Status.Hold.Of(holder)
}

// DescribeHolds says every hold with its reason: "staff: kernel debugging;
// geth".
func DescribeHolds(holds []protocol.HostHold) string {
	parts := make([]string, 0, len(holds))

	for i := range holds {
		part := holds[i].Holder
		if holds[i].Reason != "" {
			part += ": " + holds[i].Reason
		}

		parts = append(parts, part)
	}

	return strings.Join(parts, "; ")
}

// HeldReason says what holds the host, for a waiting reason: "held by the
// host, staff, geth", "the host" for the agent's local hold.
func (h *Host) HeldReason() string {
	holds := h.Holds()
	by := make([]string, 0, len(holds))

	for i := range holds {
		if holds[i].Holder == protocol.HolderHost {
			by = append(by, "the host")
		} else {
			by = append(by, holds[i].Holder)
		}
	}

	if len(by) == 0 {
		return "held"
	}

	return "held by " + strings.Join(by, ", ")
}

// OfflineMark marks a host as known offline.
type OfflineMark struct {
	// Reason says why; it is required.
	Reason string `json:"reason"`
	// Until is when the mark ends by itself; nil keeps it until a person
	// lifts it.
	Until *time.Time `json:"until,omitempty"`
}

// KnownOffline reports whether the host's offline mark is in force at now.
func (h *Host) KnownOffline(now time.Time) bool {
	return h.Offline != nil && (h.Offline.Until == nil || now.Before(*h.Offline.Until))
}

// Weight is the number of validators assigned to the host.
func (h *Host) Weight() uint64 {
	var n uint64

	for i := range h.Validators {
		n += h.Validators[i].Count()
	}

	return n
}

// staleSlack is what StaleAfter adds to two asked sync intervals: one
// request's way there and back with retries.
const staleSlack = 15 * time.Second

// StaleAfter is the silence after which the host counts as stale: the
// configured staleAfter, or two of the sync intervals the controller asked of
// the host when those are longer. A host the controller slowed down is not
// stale for doing what it was told.
func (h *Host) StaleAfter(staleAfter time.Duration) time.Duration {
	return max(staleAfter, 2*h.SyncInterval+staleSlack)
}

// Stale reports whether the host's agent has been silent for longer than
// StaleAfter(staleAfter).
func (h *Host) Stale(now time.Time, staleAfter time.Duration) bool {
	return now.Sub(h.LastSeen) > h.StaleAfter(staleAfter)
}

// Held reports whether the agent reports any hold.
func (h *Host) Held() bool {
	return h.Status != nil && h.Status.Hold.Active()
}

// AgentProtocol is the protocol version of the host's last sync; zero before
// the first.
func (h *Host) AgentProtocol() int {
	if h.Status == nil {
		return 0
	}

	return h.Status.ProtocolVersion
}

// Incompatible says why the controller serves the host nothing: its agent
// speaks a protocol version outside protocol.MinVersion..protocol.Version.
// Empty for a host it serves and one without a sync.
func (h *Host) Incompatible() string {
	if h.Status == nil {
		return ""
	}

	return Unsupported(h.Status.ProtocolVersion)
}

// Unsupported says why the controller serves no agent that speaks protocol
// version v; empty when it serves it.
func Unsupported(v int) string {
	if protocol.Supported(v) {
		return ""
	}

	return fmt.Sprintf("agent protocol %d not supported (controller supports %d..%d)",
		v, protocol.MinVersion, protocol.Version)
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
	// while they are empty or invalid (ValidateTiming) no epoch can be placed
	// in time.
	Phases []SlotPhase `json:"phases,omitempty"`
	// TimingError says why Phases is invalid (ValidateTiming); empty while it
	// is good. While it is set the network gate is closed with it as the
	// reason.
	TimingError string `json:"timingError,omitempty"`

	HeadSlot       uint64 `json:"headSlot"`
	CurrentEpoch   uint64 `json:"currentEpoch"`
	FinalizedEpoch uint64 `json:"finalizedEpoch"`
	// FinalityLagEpochs is CurrentEpoch - FinalizedEpoch. A healthy network
	// sits at 2.
	FinalityLagEpochs uint64 `json:"finalityLagEpochs"`
	// Split says how the beacon endpoints in sync divide over chains when no
	// chain is followed by a strict majority of them ("2 chains, 3/5 and 2/5
	// endpoints"); empty otherwise. While it is set the network gate is closed.
	Split string `json:"split,omitempty"`

	// LivenessEpoch is the newest settled epoch the beacon service summed
	// from the per-slot participation; LivenessKnown is false until there is
	// one. LivenessError says why the last count failed, if it did: the
	// epochs it holds up are not published.
	LivenessKnown bool   `json:"livenessKnown"`
	LivenessEpoch uint64 `json:"livenessEpoch"`
	LivenessError string `json:"livenessError,omitempty"`
	// Tracked counts the validators of all hosts with a duty in
	// LivenessEpoch, Live the ones whose attestation a block carries.
	Tracked uint64 `json:"tracked"`
	Live    uint64 `json:"live"`

	// ValidatorRangesError says why the last fetch of the validator ranges
	// inventory failed; the last good version stays in use. The controller
	// sets it, not the beacon service.
	ValidatorRangesError string `json:"validatorRangesError,omitempty"`
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

// Errors of ValidateTiming.
var (
	ErrNoTiming      = errors.New("no chain timing known")
	ErrInvalidTiming = errors.New("invalid chain timing")
)

// ValidateTiming checks the slot phases of a chain: there is at least one, an
// epoch has slots and every slot a duration, the phases are in ascending
// order of epoch, and every phase starts exactly where the one before it
// ends. The error names the first phase that fails this. A timing that fails
// places no epoch at all.
func ValidateTiming(slotsPerEpoch uint64, phases []SlotPhase) error {
	if slotsPerEpoch == 0 || len(phases) == 0 {
		return ErrNoTiming
	}

	for i := range phases {
		phase := &phases[i]
		if phase.SlotMs == 0 {
			return fmt.Errorf("%w: phase %d (from epoch %d) has slots of no duration",
				ErrInvalidTiming, i, phase.Epoch)
		}

		if i == 0 {
			continue
		}

		before := &phases[i-1]
		if phase.Epoch <= before.Epoch {
			return fmt.Errorf("%w: phase %d begins at epoch %d, not after epoch %d of the phase before it",
				ErrInvalidTiming, i, phase.Epoch, before.Epoch)
		}

		end := before.Start.Add(time.Duration(phase.Epoch-before.Epoch) * epochLength(slotsPerEpoch, before.SlotMs))
		if !phase.Start.Equal(end) {
			return fmt.Errorf("%w: phase %d (from epoch %d) starts at %s, the phase before it ends at %s",
				ErrInvalidTiming, i, phase.Epoch, phase.Start.UTC().Format(time.RFC3339Nano),
				end.UTC().Format(time.RFC3339Nano))
		}
	}

	return nil
}

// epochLength is the length of an epoch of slotsPerEpoch slots of slotMs
// milliseconds.
func epochLength(slotsPerEpoch, slotMs uint64) time.Duration {
	return time.Duration(slotsPerEpoch*slotMs) * time.Millisecond
}

// timed reports whether the view's timing places epochs: it is known and
// valid.
func (n *NetworkHealth) timed() bool {
	return ValidateTiming(n.SlotsPerEpoch, n.Phases) == nil
}

// phaseOf returns the phase the epoch lies in and the length of its epochs.
// The caller has checked timed, and that the epoch is not before the first
// phase.
func (n *NetworkHealth) phaseOf(epoch uint64) (phase *SlotPhase, length time.Duration) {
	phase = &n.Phases[0]

	for i := 1; i < len(n.Phases) && n.Phases[i].Epoch <= epoch; i++ {
		phase = &n.Phases[i]
	}

	return phase, epochLength(n.SlotsPerEpoch, phase.SlotMs)
}

// TimingStart returns the start of the first known phase: the earliest time
// that lies in an epoch. On a chain whose timing is known from genesis on it
// is the genesis time. ok is false when the timing is unknown or invalid.
func (n *NetworkHealth) TimingStart() (start time.Time, ok bool) {
	if !n.timed() {
		return time.Time{}, false
	}

	return n.Phases[0].Start, true
}

// EpochStart returns the wall-clock start of an epoch. ok is false when it
// cannot be placed: the timing is unknown or invalid, or the epoch lies
// before the first known phase.
func (n *NetworkHealth) EpochStart(epoch uint64) (start time.Time, ok bool) {
	if !n.timed() || epoch < n.Phases[0].Epoch {
		return time.Time{}, false
	}

	phase, length := n.phaseOf(epoch)

	return phase.Start.Add(time.Duration(epoch-phase.Epoch) * length), true
}

// EpochAt returns the epoch that contains t. ok is false when it cannot be
// told: the timing is unknown or invalid, or t lies before the first known
// phase.
func (n *NetworkHealth) EpochAt(at time.Time) (epoch uint64, ok bool) {
	if !n.timed() || at.Before(n.Phases[0].Start) {
		return 0, false
	}

	phase := &n.Phases[0]

	for i := 1; i < len(n.Phases) && !at.Before(n.Phases[i].Start); i++ {
		phase = &n.Phases[i]
	}

	length := epochLength(n.SlotsPerEpoch, phase.SlotMs)

	return phase.Epoch + uint64(at.Sub(phase.Start)/length), true
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

	epoch, _ := n.EpochAt(later(from, first))
	start, _ := n.EpochStart(epoch)
	end, _ := n.EpochStart(epoch + count)

	return end.Sub(start), true
}

// later returns the later of two times.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

// EpochDuration returns the length of the current epoch, or 0 when it cannot
// be placed. It is for display and for rough bounds; the time of a particular
// epoch comes from EpochStart.
func (n *NetworkHealth) EpochDuration() time.Duration {
	if !n.timed() || n.CurrentEpoch < n.Phases[0].Epoch {
		return 0
	}

	_, length := n.phaseOf(n.CurrentEpoch)

	return length
}

// Participation is Live/Tracked, or 1 when nothing is tracked.
func (n *NetworkHealth) Participation() float64 {
	if n.Tracked == 0 {
		return 1
	}

	return float64(n.Live) / float64(n.Tracked)
}
