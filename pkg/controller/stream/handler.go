// Package stream is the controller's event stream: the hub that pushes the
// views of the REST API to the web UI as server-sent events (events.go) and
// their wire encoding (wire.go).
package stream

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

// Store is the part of store.Store the streams read besides the views.
type Store interface {
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	ListAudit(ctx context.Context, filter store.AuditFilter) ([]fleet.AuditEntry, error)
	ListSlotParticipation(ctx context.Context, fromSlot uint64) ([]fleet.SlotParticipation, error)
	SumHostSlotParticipation(
		ctx context.Context, fromSlot, bucketSlots uint64,
	) ([]fleet.HostSlotBucket, error)
}

// Options are the sources of the streams.
type Options struct {
	Store     Store
	Scheduler scheduler.Service
	Beacon    views.Beacon
	Views     *views.Views
	// DefaultSettings are the settings from flags, shown next to the
	// effective ones.
	DefaultSettings fleet.Settings
	// Now is the clock.
	Now func() time.Time
}

// Handler serves the event streams. The hub runs between Start and Stop;
// before Start and after Stop a stream is refused.
type Handler struct {
	log   logrus.FieldLogger
	opts  Options
	now   func() time.Time
	views *views.Views

	// events is the hub of the event streams.
	events            eventHubs
	streamMinInterval time.Duration
	streamDebounce    time.Duration
	streamHeartbeat   time.Duration
}

// New returns the streams over opts.
//
//nolint:gocritic // hugeParam: Options by value, like the other handlers of the controller.
func New(log logrus.FieldLogger, opts Options) *Handler {
	return &Handler{
		log:   log,
		opts:  opts,
		now:   opts.Now,
		views: opts.Views,

		streamMinInterval: streamMinInterval,
		streamDebounce:    streamDebounce,
		streamHeartbeat:   streamHeartbeat,
	}
}

// Start runs the hub until Stop or the end of ctx.
func (s *Handler) Start(ctx context.Context) error {
	s.events.start(ctx, s.log, s)

	return nil
}
