// Package beacon watches the network through a beacon node: finality,
// per-epoch validator liveness for every host's validator range, and the
// chain timing the scheduler needs.
package beacon

import (
	"context"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// Service polls the beacon node. Once per epoch it asks
// POST /eth/v1/validator/liveness/{epoch} for the validator indices of all
// hosts (the previous, complete epoch) and stores the result per host.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	// Health returns the latest view. It never blocks on the network.
	Health() fleet.NetworkHealth
}
