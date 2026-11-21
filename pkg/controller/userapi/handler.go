// Package userapi is the controller's API for people: the user
// authentication, the REST views of the fleet (fleet, public, clients,
// slots, outages), the actions, the settings and the overrides.
package userapi

import (
	"context"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

const (
	// maxUserBody bounds the body of authenticated PUT and POST requests.
	maxUserBody = 64 << 10
)

// Store is the part of store.Store the user API reads besides the views.
type Store interface {
	views.Store
	authz.Store

	ListPackages(ctx context.Context, filter store.PackageFilter) (*store.PackagePage, error)
	ListSlotParticipation(ctx context.Context, fromSlot uint64) ([]fleet.SlotParticipation, error)
	SumHostSlotRuns(
		ctx context.Context, fromSlot, toSlot, gap, minSlots uint64, perSlot []string,
	) ([]fleet.HostSlotRun, error)
}

// Options configures the user API.
type Options struct {
	// Network is the devnet name; Version the controller build version.
	Network string
	Version string
	// AuthProviderURL is the authenticatoor base URL without a trailing
	// slash. Empty means every user route is open and the audit actor is
	// "anonymous".
	AuthProviderURL string
	// AuthService is the name of this service in the token's "services"
	// claim.
	AuthService string

	Store     Store
	Scheduler scheduler.Service
	// Lookups looks the hosts' tags up at the registries; nil without them.
	Lookups Lookups
	Beacon  views.Beacon
	Views   *views.Views
	// DefaultSettings are the settings from flags, shown next to the
	// effective ones.
	DefaultSettings fleet.Settings
	// Notify says that an action or a settings change changed what the views
	// show.
	Notify func()
	// Now is the clock.
	Now func() time.Time
}

// Lookups is the part of discovery.Tracker the user routes use.
type Lookups interface {
	Refresh() discovery.Refreshed
}

// Handler serves the user routes and the public REST views.
type Handler struct {
	log   logrus.FieldLogger
	opts  Options
	now   func() time.Time
	views *views.Views
	users *userAuth
	authz authz.Authorizer
}

// New returns the user API over opts.
//
//nolint:gocritic // hugeParam: Options by value, like the other handlers of the controller.
func New(log logrus.FieldLogger, opts Options) *Handler {
	return &Handler{
		log:   log,
		opts:  opts,
		now:   opts.Now,
		views: opts.Views,
		authz: authz.New(log.WithField("package", "authz"), opts.AuthService, opts.Store, opts.Now),
		users: newUserAuth(opts.AuthProviderURL, opts.AuthService, opts.Now),
	}
}

// Start builds the user token verifier when an auth provider is configured.
// ctx bounds the discovery; refreshCtx is the lifetime of the background key
// refresh.
func (s *Handler) Start(ctx, refreshCtx context.Context) error {
	return s.users.start(ctx, refreshCtx, s.log)
}

// Route is one route of the user API. Exactly one of Public and User is set:
// a User handler is only served behind Guard.
type Route struct {
	Method  string
	Pattern string
	Public  http.HandlerFunc
	User    UserHandler
}

// Routes are the routes of the user API: the public views first, then the
// routes behind the user authentication.
func (s *Handler) Routes() []Route {
	routes := []Route{
		{Method: http.MethodGet, Pattern: "/api/v1/config", Public: s.handleConfig},
		{Method: http.MethodGet, Pattern: "/api/v1/public/hosts", Public: s.handlePublicHosts},
		{Method: http.MethodGet, Pattern: "/api/v1/public/images", Public: s.handlePublicImages},
		{Method: http.MethodGet, Pattern: "/api/v1/public/network", Public: s.handlePublicNetwork},
		{Method: http.MethodGet, Pattern: "/api/v1/public/slots", Public: s.handlePublicSlots},

		{Method: http.MethodGet, Pattern: "/api/v1/hosts", User: s.handleHosts},
		{Method: http.MethodGet, Pattern: "/api/v1/hosts/{name}", User: s.handleHost},
		{Method: http.MethodGet, Pattern: "/api/v1/clients", User: s.handleClients},
		{Method: http.MethodGet, Pattern: "/api/v1/clients/{client}/hosts", User: s.handleClientHosts},
		{Method: http.MethodGet, Pattern: "/api/v1/packages", User: s.handlePackages},
		{Method: http.MethodGet, Pattern: "/api/v1/images", User: s.handleImages},
		{Method: http.MethodPost, Pattern: "/api/v1/images/refresh", User: s.handleRefreshImages},
		{Method: http.MethodGet, Pattern: "/api/v1/rollouts", User: s.handleRollouts},
		{Method: http.MethodGet, Pattern: "/api/v1/plan", User: s.handlePlan},
		{Method: http.MethodGet, Pattern: "/api/v1/settings", User: s.handleGetSettings},
		{Method: http.MethodPut, Pattern: "/api/v1/settings", User: s.handlePutSettings},
		{Method: http.MethodGet, Pattern: "/api/v1/audit", User: s.handleAudit},
		{Method: http.MethodGet, Pattern: "/api/v1/slots", User: s.handleSlots},
	}

	actions := s.actions()
	out := make([]Route, 0, len(routes)+len(actions))
	out = append(out, routes...)

	for name, handler := range actions {
		out = append(out, Route{Method: http.MethodPost, Pattern: "/api/v1/actions/" + name, User: handler})
	}

	return out
}
