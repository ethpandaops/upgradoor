// Package stream is the controller's event stream: the hub that pushes the
// views of the REST API to the web UI as server-sent events (events.go) and
// their wire encoding (wire.go).
package stream

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

// Store is the part of store.Store the streams read besides the views.
type Store interface {
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	ListAudit(ctx context.Context, filter store.AuditFilter) ([]fleet.AuditEntry, error)
	SSHLoginsSeen(ctx context.Context) (bool, error)
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
	// Access is how much of the hosts a login reads; nil reduces every login
	// but the open one.
	Access func(u *authz.User) views.Access
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

// access is how much of the hosts u reads.
func (s *Handler) access(u *authz.User) views.Access {
	switch {
	case s.opts.Access != nil:
		return s.opts.Access(u)
	case u.Open:
		return views.Access{Full: true, Admin: true, AuditLog: true}
	default:
		return views.Access{}
	}
}

// Start runs the hub until Stop or the end of ctx.
func (s *Handler) Start(ctx context.Context) error {
	s.events.start(ctx, s.log, s)

	return nil
}
