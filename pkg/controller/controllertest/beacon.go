package controllertest

import "github.com/ethpandaops/upgradoor/pkg/controller/fleet"

// Beacon is a beacon service with a fixed view of the network.
type Beacon struct {
	Network fleet.NetworkHealth
}

// Health returns Network.
func (f *Beacon) Health() fleet.NetworkHealth { return f.Network }
