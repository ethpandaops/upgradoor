// Package api is the controller's HTTP server: it validates the options and
// mounts the agent API (agentapi), the user API (userapi) and the event
// streams (stream), next to the health check, the metrics and the web UI.
package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/agentapi"
	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/metrics"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/stream"
	"github.com/ethpandaops/upgradoor/pkg/controller/userapi"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/controller/webui"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Store is the part of store.Store the API needs.
type Store interface {
	RecordSync(ctx context.Context, sync *store.HostSync) (*store.SyncResult, error)
	GetHost(ctx context.Context, name string) (*fleet.Host, error)
	ListHosts(ctx context.Context) ([]*fleet.Host, error)

	SaveInventory(ctx context.Context, host string, inv *protocol.Inventory) error
	GetInventory(ctx context.Context, host string) (*protocol.Inventory, error)
	ListPackages(ctx context.Context, filter store.PackageFilter) (*store.PackagePage, error)

	// ListSlotParticipation returns the slot rows from fromSlot on, oldest
	// first; SumHostSlotParticipation their per-host part summed over
	// buckets of bucketSlots slots.
	ListSlotParticipation(ctx context.Context, fromSlot uint64) ([]fleet.SlotParticipation, error)
	SumHostSlotParticipation(
		ctx context.Context, fromSlot, bucketSlots uint64,
	) ([]fleet.HostSlotBucket, error)
	// SumHostSlotRuns returns the runs of slots from fromSlot to toSlot in
	// which a host missed attestations, ordered by host and first slot. Rows
	// of a host no more than gap slots apart are one run, and only runs of at
	// least minSlots slots are returned; for the hosts in perSlot every row
	// is a run of its own, and all of them are returned.
	SumHostSlotRuns(
		ctx context.Context, fromSlot, toSlot, gap, minSlots uint64, perSlot []string,
	) ([]fleet.HostSlotRun, error)
	ListRollouts(ctx context.Context, filter store.RolloutFilter) ([]*fleet.Rollout, error)
	ListLeases(ctx context.Context, filter store.LeaseFilter) ([]*fleet.Lease, error)
	// GetRollout returns one rollout or store.ErrNotFound.
	GetRollout(ctx context.Context, id string) (*fleet.Rollout, error)
	// GetLease returns one lease or store.ErrNotFound.
	GetLease(ctx context.Context, id string) (*fleet.Lease, error)
	// ListDirectives returns directives, oldest first.
	ListDirectives(ctx context.Context, filter store.DirectiveFilter) ([]*fleet.Directive, error)

	AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error
	ListAudit(ctx context.Context, filter store.AuditFilter) ([]fleet.AuditEntry, error)
}

// Beacon is the part of beacon.Service the API needs.
type Beacon interface {
	Health() fleet.NetworkHealth
}

// DefaultAuthService is the name this binary answers to in the token's
// "services" claim unless Options.AuthService names another.
const DefaultAuthService = "upgradoor"

// Registry is where the API registers its metrics and what /metrics serves.
// *prometheus.Registry implements it.
type Registry interface {
	prometheus.Registerer
	prometheus.Gatherer
}

// Options configures the API server.
type Options struct {
	// Network is the devnet name; certificates for another network are
	// refused by Verifier.
	Network string
	// Version is the controller build version.
	Version string

	// Verifier authenticates agent requests.
	Verifier *identity.Verifier
	// PublicURLs are the base URLs (scheme and host, optionally a path
	// prefix) under which agents reach the controller. A request proof must
	// name one of them. When empty the base is derived from the request's
	// Host header: "https://<host>", and "http://<host>" only with
	// AllowInsecureAgentHTTP.
	PublicURLs []string
	// AllowInsecureAgentHTTP accepts proofs for plain http URLs (local
	// development).
	AllowInsecureAgentHTTP bool
	// AnswerSigner signs every answer of the agent API with the controller
	// key. Nil leaves them unsigned, which only an agent that runs without
	// controller keys takes (development only; the controller wiring demands
	// an explicit opt-out for it).
	AnswerSigner *identity.AnswerSigner
	// TrustForwardedHeaders takes the remote address from X-Forwarded-For.
	// Set it only behind a proxy that sets the header itself.
	TrustForwardedHeaders bool

	// AuthProviderURL is the authenticatoor base URL. Empty means every user
	// route is open and the audit actor is "anonymous".
	AuthProviderURL string
	// AuthService is the name of this service in the token's "services"
	// claim: the token must have basic access to it, and the features that
	// allow actions are features of it. Empty means DefaultAuthService.
	AuthService string
	// UIOrigins are extra origins the web UI may load scripts from and
	// connect to.
	UIOrigins []string

	Store     Store
	Scheduler scheduler.Service
	Beacon    Beacon
	// Endpoints and Validators are the data sources the settings page shows;
	// nil shows none.
	Endpoints  userapi.BeaconEndpoints
	Validators userapi.ValidatorRanges
	// Lookups says what the tags of the hosts point at.
	Lookups discovery.Tracker
	// DefaultSettings are the settings from flags, shown next to the
	// effective ones.
	DefaultSettings fleet.Settings

	// WebFS is the built web UI; nil serves a placeholder page.
	WebFS fs.FS
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Registry receives the API's metrics; nil creates a private registry.
	Registry Registry
}

// Server is the controller's HTTP API. It mounts the agent API, the user API
// and the event streams.
//
// Handler() can be used right after New. When an auth provider is configured,
// user routes answer 503 until Start has built the token verifier; the agent
// and public routes do not depend on it.
type Server struct {
	log  logrus.FieldLogger
	opts Options

	handler http.Handler
	views   *views.Views
	agents  *agentapi.Handler
	users   *userapi.Handler
	streams *stream.Handler
	// userRoutes lists the routes behind the user authentication, as
	// "METHOD pattern". The route tests walk it, so a new route cannot miss
	// them.
	userRoutes []string

	mu     sync.Mutex
	cancel context.CancelFunc
}

// New validates the options and builds the routes.
//
//nolint:gocritic // hugeParam: Options by value is the signature the controller wiring is built against.
func New(log logrus.FieldLogger, opts Options) (*Server, error) {
	if log == nil {
		return nil, errors.New("api: logger is required")
	}

	if err := identity.ValidateName(opts.Network); err != nil {
		return nil, fmt.Errorf("api: network: %w", err)
	}

	switch {
	case opts.Verifier == nil:
		return nil, errors.New("api: verifier is required")
	case opts.Store == nil:
		return nil, errors.New("api: store is required")
	case opts.Scheduler == nil:
		return nil, errors.New("api: scheduler is required")
	case opts.Beacon == nil:
		return nil, errors.New("api: beacon is required")
	}

	if err := opts.DefaultSettings.Validate(); err != nil {
		return nil, fmt.Errorf("api: default settings: %w", err)
	}

	opts.AuthProviderURL = strings.TrimRight(opts.AuthProviderURL, "/")

	if opts.AuthService == "" {
		opts.AuthService = DefaultAuthService
	}

	if !authz.AuthServicePattern.MatchString(opts.AuthService) {
		return nil, fmt.Errorf("api: auth service %q is not a service name "+
			"([a-z0-9][a-z0-9_-]{0,62})", opts.AuthService)
	}

	publicURLs, err := agentapi.NormalizePublicURLs(opts.PublicURLs, opts.AllowInsecureAgentHTTP)
	if err != nil {
		return nil, err
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Registry == nil {
		opts.Registry = prometheus.NewRegistry()
	}

	s := &Server{
		log:  log.WithField("package", "api"),
		opts: opts,
		views: views.New(views.Options{
			Store: opts.Store, Scheduler: opts.Scheduler, Beacon: opts.Beacon, Now: opts.Now,
		}),
	}

	collector, err := metrics.New(s.log, opts.Registry, metrics.Options{
		Store: opts.Store, Scheduler: opts.Scheduler, Gate: s.views.Gate, Now: opts.Now,
	})
	if err != nil {
		return nil, err
	}

	s.streams = stream.New(s.log, stream.Options{
		Store: opts.Store, Scheduler: opts.Scheduler, Beacon: opts.Beacon, Views: s.views,
		Access: func(u *authz.User) views.Access { return s.users.Access(u) },
		Now:    opts.Now,
	})
	s.agents = agentapi.New(s.log, agentapi.Options{
		Verifier: opts.Verifier, PublicURLs: publicURLs,
		AllowInsecureAgentHTTP: opts.AllowInsecureAgentHTTP, AnswerSigner: opts.AnswerSigner,
		TrustForwardedHeaders: opts.TrustForwardedHeaders,
		Store:                 opts.Store, Scheduler: opts.Scheduler, Lookups: opts.Lookups,
		Metrics: collector,
		Notify:  s.Notify, NotifyHost: s.streams.NotifyHost, Now: opts.Now,
	})
	s.users = userapi.New(s.log, userapi.Options{
		Lookups: opts.Lookups,
		Network: opts.Network, Version: opts.Version,
		AuthProviderURL: opts.AuthProviderURL, AuthService: opts.AuthService,
		Store: opts.Store, Scheduler: opts.Scheduler, Beacon: opts.Beacon, Views: s.views,
		Endpoints: opts.Endpoints, Validators: opts.Validators,
		DefaultSettings: opts.DefaultSettings, Notify: s.Notify, Now: opts.Now,
	})

	ui, err := webui.New(log, opts.WebFS, webui.Options{
		AuthProviderURL: opts.AuthProviderURL,
		ExtraOrigins:    opts.UIOrigins,
	})
	if err != nil {
		return nil, fmt.Errorf("api: web UI: %w", err)
	}

	s.handler = s.routes(ui)

	return s, nil
}

// Handler returns the HTTP handler for every route.
func (s *Server) Handler() http.Handler { return s.handler }

// Start builds the user token verifier when an auth provider is configured
// and starts the hub of the event streams. The verifier refreshes its keys in
// the background until Stop.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		return errors.New("api: already started")
	}

	// The key refresh must outlive the start-up context, so it gets its own,
	// cancelled by Stop.
	refreshCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	if err := s.users.Start(ctx, refreshCtx); err != nil {
		cancel()

		return err
	}

	if err := s.streams.Start(refreshCtx); err != nil {
		cancel()

		return err
	}

	s.cancel = cancel

	return nil
}

// Stop ends the background key refresh and closes the event streams.
func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		_ = s.streams.Stop()
		s.cancel()
		s.cancel = nil
	}

	return nil
}

// Notify says that state the API serves may have changed. It never blocks,
// so it can be called under any lock. The scheduler is wired to it.
func (s *Server) Notify() {
	s.streams.Notify()
}

// CloseStreams ends every open event stream and refuses new ones. An open
// stream never becomes idle, so an http.Server that serves the handler cannot
// shut down gracefully before this was called. Stop calls it too.
func (s *Server) CloseStreams() {
	_ = s.streams.Stop()
}

// routes builds the mux. Everything else, a known path with another method
// included, goes to the web UI, which answers unknown /api/ paths with a JSON
// 404.
func (s *Server) routes(ui http.Handler) http.Handler {
	mux := http.NewServeMux()

	handle := func(method, pattern string, handler http.Handler) {
		mux.Handle(method+" "+pattern, handler)
	}

	// A user handler can only be registered through here, behind the user
	// authentication.
	user := func(method, pattern string, handler userapi.UserHandler) {
		handle(method, pattern, s.users.Guard(handler))

		s.userRoutes = append(s.userRoutes, method+" "+pattern)
	}

	for _, route := range s.agents.Routes() {
		handle(route.Method, route.Pattern, route.Handler)
	}

	for _, route := range s.users.Routes() {
		if route.User != nil {
			user(route.Method, route.Pattern, route.User)
		} else {
			handle(route.Method, route.Pattern, route.Public)
		}
	}

	handle(http.MethodGet, stream.PathPublicEvents, http.HandlerFunc(s.streams.HandlePublic))
	user(http.MethodGet, stream.PathEvents, s.streams.Handle)

	handle(http.MethodGet, "/healthz", http.HandlerFunc(s.handleHealthz))
	handle(http.MethodGet, "/metrics", promhttp.HandlerFor(s.opts.Registry, promhttp.HandlerOpts{
		ErrorLog:            s.log,
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: 4,
		Timeout:             20 * time.Second,
	}))

	mux.Handle("/", ui)

	return apiHeaders(mux)
}

// apiHeaders sets the headers every API answer carries. No CORS header is
// ever set: the API is for the same-origin UI and for agents.
func apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/healthz" {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "no-store")
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	httpjson.Write(s.log, w, http.StatusOK, map[string]string{"status": "ok"})
}
