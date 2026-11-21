// Package fleet is the controller's domain model: hosts, needs, rollouts,
// leases, settings, the audit trail and network health. It holds types and
// small pure helpers only; persistence lives in store, decisions in
// scheduler.
package fleet

import (
	"errors"
	"fmt"
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
	// Generation and Roles are copied from the certificate.
	Generation uint64 `json:"generation"`
	// Validators is the range of validators the controller counts for the
	// host: its weight, and the validators whose attestations are tracked
	// for it. It comes from what the host reports (Status.Validators), not
	// from the certificate, and differs from the report in two cases. While
	// the host does not know its range (its marker file is missing or
	// unreadable) the range counted last is kept. And no validator is
	// counted for two hosts: where the reported range runs into validators
	// that are counted for another host, only the part nobody else holds is
	// counted (UnclaimedRange). The ranges of all hosts are therefore
	// disjoint.
	Validators *identity.ValidatorRange `json:"validators,omitempty"`
	Roles      []string                 `json:"roles"`

	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	// RemoteAddr is the address the last sync came from.
	RemoteAddr string `json:"remoteAddr,omitempty"`

	// Status is the last sync request, verbatim.
	Status *protocol.SyncRequest `json:"status,omitempty"`
	// StatusHash identifies Status without its clock fields (SentAt,
	// UptimeSeconds); empty when the store keeps none.
	StatusHash string `json:"-"`
	// InventoryHash is the hash of the inventory stored for this host.
	InventoryHash string `json:"inventoryHash,omitempty"`

	// HoldWanted is the controller-side hold switch; the scheduler turns it
	// into hold / release-hold directives until the agent agrees.
	HoldWanted bool   `json:"holdWanted"`
	HoldReason string `json:"holdReason,omitempty"`
	// HoldBy says through what the hold was set: HoldByTeam when a person
	// set it with a team feature, empty for every other hold. A team feature
	// lifts only a hold that was set through one.
	HoldBy string `json:"holdBy,omitempty"`
	// HoldActor is the person who set the controller hold, as the audit log
	// names them; the agent writes them into the hold entry of its overrides
	// file.
	HoldActor string `json:"holdActor,omitempty"`

	// Offline is a person's mark that the host is known offline, for example
	// a client kept broken on purpose for debugging; nil while unmarked. While
	// it is in force (KnownOffline) the host's validators are left out of the
	// gate's participation and the budget, and the host gets no lease.
	Offline *OfflineMark `json:"offline,omitempty"`

	// Broken says what the host's last lease failed on: per group of rollouts
	// (a roll group as "group:<group>#<client>", or "packages") the ID of the
	// lease that failed. A host that is broken for the group of a rollout goes
	// first in it. The scheduler sets the mark where a lease fails and clears
	// it where a lease is released with the group's steps done; it goes with
	// the host when the host is forgotten.
	Broken map[string]string `json:"broken,omitempty"`
	// Pull is the host's pull window: the images the scheduler lets it pull
	// now. Nil while it may pull nothing.
	Pull *PullWindow `json:"pull,omitempty"`

	// SyncInterval is the longest sync interval the controller asked of the
	// host in its last answer. StaleAfter allows for it.
	SyncInterval time.Duration `json:"syncInterval,omitempty"`
}

// PullWindow is a host's permission to pull the images its needs wait for.
type PullWindow struct {
	Images    []protocol.PullImage `json:"images"`
	GrantedAt time.Time            `json:"grantedAt"`
	// EndedAt is when the scheduler ended the window. It is no longer
	// offered, and counts for the limit until the agent synced after it
	// without pulling.
	EndedAt *time.Time `json:"endedAt,omitempty"`
}

// Open reports whether the window is offered to the agent.
func (w *PullWindow) Open() bool { return w != nil && w.EndedAt == nil }

// HoldByTeam is Host.HoldBy for a hold set through a team feature.
const HoldByTeam = "team"

// OfflineMark marks a host as known offline.
type OfflineMark struct {
	// Reason says why; it is required.
	Reason string `json:"reason"`
	// Until is when the mark ends by itself; nil keeps it until a person
	// lifts it.
	Until *time.Time `json:"until,omitempty"`
	// By is HoldByTeam for a mark set through a team feature: a team feature
	// lifts only such a mark.
	By string `json:"by,omitempty"`
}

// KnownOffline reports whether the host's offline mark is in force at now.
func (h *Host) KnownOffline(now time.Time) bool {
	return h.Offline != nil && (h.Offline.Until == nil || now.Before(*h.Offline.Until))
}

// Weight is the number of validators counted for the host.
func (h *Host) Weight() uint64 { return h.Validators.Count() }

// ReportedValidators is the validator range the host's last sync stated, or
// nil when it stated none or does not know it.
func (h *Host) ReportedValidators() *identity.ValidatorRange {
	if h.Status == nil || h.Status.Validators.Count() == 0 {
		return nil
	}

	return &identity.ValidatorRange{
		Start: h.Status.Validators.Start, End: h.Status.Validators.End,
	}
}

// ReportedOverlaps names the hosts of all, other than h, whose reported
// validator range shares validators with the one h reports, ordered as all
// is. Two hosts say they run the same validators: for a moment while keys
// are moved between them, or because one of them is wrong.
func (h *Host) ReportedOverlaps(all []*Host) []string {
	mine := h.ReportedValidators()
	if mine == nil {
		return nil
	}

	var out []string

	for _, other := range all {
		if other == nil || other.Name == h.Name {
			continue
		}

		theirs := other.ReportedValidators()
		if theirs != nil && theirs.Start < mine.End && mine.Start < theirs.End {
			out = append(out, other.Name)
		}
	}

	return out
}

// UnclaimedRange returns the largest part of want that none of taken covers,
// the first one of several of that size; nil when all of want is taken or
// want is empty. taken must be ordered by Start.
//
// It decides what is counted for a host whose reported range overlaps what
// is counted for other hosts: whoever held a validator first keeps it, so a
// host cannot take validators away from another by reporting them.
func UnclaimedRange(
	want identity.ValidatorRange, taken []identity.ValidatorRange,
) *identity.ValidatorRange {
	var best *identity.ValidatorRange

	consider := func(start, end uint64) {
		if end > start && (best == nil || end-start > best.End-best.Start) {
			best = &identity.ValidatorRange{Start: start, End: end}
		}
	}

	cursor := want.Start

	for _, t := range taken {
		if t.End <= cursor || t.End <= t.Start {
			continue
		}

		if t.Start >= want.End {
			break
		}

		consider(cursor, min(t.Start, want.End))

		cursor = t.End
		if cursor >= want.End {
			return best
		}
	}

	consider(cursor, want.End)

	return best
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
