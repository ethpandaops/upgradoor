// Package fleet is the controller's domain model: hosts, needs, rollouts,
// leases, settings, the audit trail and network health. It holds types and
// small pure helpers only; persistence lives in store, decisions in
// scheduler.
package fleet

import (
	"time"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// RoleBootnode is the certificate role that marks a bootnode. Bootnodes are
// rolled last and one at a time.
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

// IsBootnode reports whether the host carries the bootnode role.
func (h *Host) IsBootnode() bool {
	for _, r := range h.Roles {
		if r == RoleBootnode {
			return true
		}
	}

	return false
}

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

	GenesisTime    time.Time `json:"genesisTime"`
	SecondsPerSlot uint64    `json:"secondsPerSlot"`
	SlotsPerEpoch  uint64    `json:"slotsPerEpoch"`

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
	// Tracked and Live count the validators of all hosts in LivenessEpoch.
	Tracked uint64 `json:"tracked"`
	Live    uint64 `json:"live"`
}

// EpochDuration returns the length of one epoch, or 0 when unknown.
func (n *NetworkHealth) EpochDuration() time.Duration {
	return time.Duration(n.SecondsPerSlot) * time.Duration(n.SlotsPerEpoch) * time.Second
}

// Participation is Live/Tracked, or 1 when nothing is tracked.
func (n *NetworkHealth) Participation() float64 {
	if n.Tracked == 0 {
		return 1
	}

	return float64(n.Live) / float64(n.Tracked)
}
