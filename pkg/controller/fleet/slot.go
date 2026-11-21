package fleet

import "time"

// SlotParticipation is what the tracked validators (every host's range) did
// in one slot: how many of them had to attest in it and how many of those
// attestations a canonical block carries, and whether the slot has a block
// at all. It is for display; nothing is scheduled from it.
type SlotParticipation struct {
	Slot uint64 `json:"slot"`
	// Proposed is false when the slot has no canonical block.
	Proposed bool `json:"proposed"`
	// Assigned counts the tracked validators with an attestation duty in the
	// slot, Included the ones whose attestation is in a canonical block. A
	// validator in the ranges of two hosts counts once here.
	Assigned uint64 `json:"assigned"`
	Included uint64 `json:"included"`
	// Hosts splits the counts per host. A host without a duty in the slot is
	// left out.
	Hosts []HostSlotParticipation `json:"hosts,omitempty"`
}

// HostSlotParticipation is one host's part of a SlotParticipation.
type HostSlotParticipation struct {
	Host     string `json:"host"`
	Assigned uint64 `json:"assigned"`
	Included uint64 `json:"included"`
}

// HostSlotBucket sums one host's slot participation over a bucket of slots:
// the slots [Bucket*n, (Bucket+1)*n) for the bucket size n it was asked for.
type HostSlotBucket struct {
	Bucket   uint64
	Host     string
	Assigned uint64
	Included uint64
}

// HostSlotRun is a run of slots in which a host missed attestations: rows
// no more than a gap apart, from First to Last. Slots is the number of slots
// of the run with a row; Assigned and Included sum them.
type HostSlotRun struct {
	Host        string
	First, Last uint64
	Slots       uint64
	Assigned    uint64
	Included    uint64
}

// SlotAt returns the slot that contains at. ok is false when it cannot be
// told: see EpochAt.
func (n *NetworkHealth) SlotAt(at time.Time) (slot uint64, ok bool) {
	epoch, ok := n.EpochAt(at)
	if !ok {
		return 0, false
	}

	start, ok := n.EpochStart(epoch)
	if !ok {
		return 0, false
	}

	length, ok := n.SlotDuration(epoch)
	if !ok {
		return 0, false
	}

	// Inside one epoch: at most MaxSlotsPerEpoch slots further.
	return epoch*n.SlotsPerEpoch + uint64(at.Sub(start)/length), true
}

// SlotStart returns the wall-clock start of a slot. ok is false when its
// epoch cannot be placed: see EpochStart.
func (n *NetworkHealth) SlotStart(slot uint64) (start time.Time, ok bool) {
	if n.SlotsPerEpoch == 0 {
		return time.Time{}, false
	}

	epoch := slot / n.SlotsPerEpoch

	start, ok = n.EpochStart(epoch)
	if !ok {
		return time.Time{}, false
	}

	length, ok := n.SlotDuration(epoch)
	if !ok {
		return time.Time{}, false
	}

	return start.Add(time.Duration(slot%n.SlotsPerEpoch) * length), true
}

// SlotDuration returns the length of a slot of the epoch. ok is false when
// the epoch cannot be placed.
func (n *NetworkHealth) SlotDuration(epoch uint64) (length time.Duration, ok bool) {
	if !n.timed() || epoch < n.Phases[0].Epoch {
		return 0, false
	}

	phase, _ := n.phaseOf(epoch)

	return time.Duration(phase.SlotMs) * time.Millisecond, true
}
