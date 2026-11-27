// Package views builds what the controller shows of the fleet: the JSON
// shapes of the REST API (schema.go) and the views the REST handlers and the
// event stream share, from the store, the scheduler and the beacon service.
package views

import (
	"context"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/upstream"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Store is the part of store.Store the views read.
type Store interface {
	GetHost(ctx context.Context, name string) (*fleet.Host, error)
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	GetInventory(ctx context.Context, host string) (*protocol.Inventory, error)
	ListRollouts(ctx context.Context, filter store.RolloutFilter) ([]*fleet.Rollout, error)
	ListLeases(ctx context.Context, filter store.LeaseFilter) ([]*fleet.Lease, error)
	ListAudit(ctx context.Context, filter store.AuditFilter) ([]fleet.AuditEntry, error)
}

// Beacon is the part of beacon.Service the views read.
type Beacon interface {
	Health() fleet.NetworkHealth
}

// Sources is the part of upstream.Tracker the views read.
type Sources interface {
	Status(image string) *upstream.Status
	Commits(image string, digests []string) map[string]string
}

// Options are the sources of the views.
type Options struct {
	Store     Store
	Scheduler scheduler.Service
	Beacon    Beacon
	// Sources says how an image stands against the branch it is built from;
	// nil knows of none.
	Sources Sources
	// Now is the clock.
	Now func() time.Time
}

// Views builds the views that need the store, the scheduler or the beacon
// service. It holds no state of its own.
type Views struct {
	opts Options
}

// New returns the views over opts.
func New(opts Options) *Views {
	return &Views{opts: opts}
}

func (v *Views) now() time.Time {
	return v.opts.Now()
}
