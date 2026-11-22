// Package beacon watches the network through a beacon node: finality, the
// chain timing the scheduler needs, and what every host's validators did slot
// by slot (slots.go), summed per epoch into their liveness (liveness.go).
package beacon

import (
	"context"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// Service polls the beacon node. Once per slot it reads the new blocks of
// the canonical chain and stores which of the hosts' attestations they carry;
// once per epoch it reads the attestation committees.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	// Health returns the latest view. It never blocks on the network.
	Health() fleet.NetworkHealth
	// Liveness returns the per-host liveness of the newest settled epochs,
	// at most epochs of them, newest epoch first. It never blocks on the
	// network.
	Liveness(epochs int) []fleet.Liveness
	// AttestedSlots returns per host the newest slots, ascending, for which
	// a canonical block carries an attestation of its validators, at most
	// fleet.MaxReadyAttestations of them. The map and its slices must not be
	// changed. It never blocks on the network.
	AttestedSlots() map[string][]uint64
	// Endpoints returns what was seen of every configured endpoint, in the
	// configured order. It never blocks on the network.
	Endpoints() []EndpointStatus
}
