package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/webui"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Suggested limits for the http.Server that serves Handler(). The handler
// bounds every body it reads; these bound the time a client may take. The
// agent endpoints additionally put their own deadline on reading the body.
// WriteTimeout must leave room for an inventory upload (up to
// protocol.MaxInventoryBody) over a slow link.
const (
	// ReadHeaderTimeout bounds the time to read the request headers.
	ReadHeaderTimeout = 10 * time.Second
	// ReadTimeout bounds the time to read a whole request.
	ReadTimeout = 60 * time.Second
	// WriteTimeout bounds the time from the end of the headers to the end of
	// the answer.
	WriteTimeout = 90 * time.Second
	// IdleTimeout bounds how long a keep-alive connection may sit idle.
	IdleTimeout = 120 * time.Second
	// MaxHeaderBytes bounds the request headers. A certificate and a proof
	// are well below 2 KiB together.
	MaxHeaderBytes = 32 << 10
)

const (
	// maxUserBody bounds the body of authenticated PUT and POST requests.
	maxUserBody = 64 << 10

	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json; charset=utf-8"

	// agentPathPrefix is the root of the agent API.
	agentPathPrefix = "/api/agent/"

	// actorAnonymous is the audit actor when the API runs open.
	actorAnonymous = "anonymous"
)

// Store is the part of store.Store the API needs.
type Store interface {
	RecordSync(ctx context.Context, sync *store.HostSync) (*store.SyncResult, error)
	GetHost(ctx context.Context, name string) (*fleet.Host, error)
	ListHosts(ctx context.Context) ([]*fleet.Host, error)

	SaveInventory(ctx context.Context, host string, inv *protocol.Inventory) error
	GetInventory(ctx context.Context, host string) (*protocol.Inventory, error)
	SearchPackages(ctx context.Context, query string, limit int) ([]store.PackageMatch, error)

	ListLiveness(ctx context.Context, epochs int) ([]fleet.Liveness, error)
	ListRollouts(ctx context.Context, filter store.RolloutFilter) ([]*fleet.Rollout, error)
	ListLeases(ctx context.Context, filter store.LeaseFilter) ([]*fleet.Lease, error)
	// GetRollout returns one rollout or store.ErrNotFound.
	GetRollout(ctx context.Context, id string) (*fleet.Rollout, error)
	// GetLease returns one lease or store.ErrNotFound.
	GetLease(ctx context.Context, id string) (*fleet.Lease, error)

	AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error
	ListAudit(ctx context.Context, filter store.AuditFilter) ([]fleet.AuditEntry, error)
}

// Beacon is the part of beacon.Service the API needs.
type Beacon interface {
	Health() fleet.NetworkHealth
}

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

	Store     Store
	Scheduler scheduler.Service
	Beacon    Beacon
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

// Server is the controller's HTTP API.
//
// Handler() can be used right after New. When an auth provider is configured,
// user routes answer 503 until Start has built the token verifier; the agent
// and public routes do not depend on it.
type Server struct {
	log  logrus.FieldLogger
	opts Options
	now  func() time.Time

	publicURLs []string
	handler    http.Handler
	metrics    *metrics
	users      *userAuth
	// userRoutes lists the routes behind the user authentication, as
	// "METHOD pattern". The route tests walk it, so a new route cannot miss
	// them.
	userRoutes []string

	syncSlots            chan struct{}
	inventorySlots       chan struct{}
	syncReadTimeout      time.Duration
	inventoryReadTimeout time.Duration

	// auditBacklog holds identity audit entries whose write failed.
	auditMu      sync.Mutex
	auditBacklog []fleet.AuditEntry

	// generationAudited remembers, per host, when a certificate that was
	// rejected for its generation was last audited.
	generationMu      sync.Mutex
	generationAudited map[string]time.Time

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

	if !authServicePattern.MatchString(opts.AuthService) {
		return nil, fmt.Errorf("api: auth service %q is not a service name "+
			"([a-z0-9][a-z0-9_-]{0,62})", opts.AuthService)
	}

	publicURLs, err := normalizePublicURLs(opts.PublicURLs, opts.AllowInsecureAgentHTTP)
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
		log:            log.WithField("package", "api"),
		opts:           opts,
		now:            opts.Now,
		publicURLs:     publicURLs,
		users:          newUserAuth(opts.AuthProviderURL, opts.AuthService, opts.Now),
		syncSlots:      make(chan struct{}, maxConcurrentSyncs),
		inventorySlots: make(chan struct{}, maxConcurrentInventories),

		syncReadTimeout:      syncReadTimeout,
		inventoryReadTimeout: inventoryReadTimeout,
	}

	s.metrics, err = newMetrics(s.log, opts.Registry, s)
	if err != nil {
		return nil, err
	}

	ui, err := webui.New(log, opts.WebFS, webui.Options{AuthProviderURL: opts.AuthProviderURL})
	if err != nil {
		return nil, fmt.Errorf("api: web UI: %w", err)
	}

	s.handler = s.routes(ui)

	return s, nil
}

// normalizePublicURLs checks the configured base URLs and strips trailing
// slashes.
func normalizePublicURLs(raw []string, allowHTTP bool) ([]string, error) {
	out := make([]string, 0, len(raw))

	for _, base := range raw {
		u, err := url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("api: public URL %q: %w", base, err)
		}

		if u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("api: public URL %q must be scheme://host[/prefix]", base)
		}

		switch u.Scheme {
		case "https":
		case "http":
			if !allowHTTP {
				return nil, fmt.Errorf(
					"api: public URL %q is plain http; allow insecure agent HTTP to accept it",
					base)
			}
		default:
			return nil, fmt.Errorf("api: public URL %q must be http or https", base)
		}

		out = append(out, strings.TrimRight(base, "/"))
	}

	return out, nil
}

// Handler returns the HTTP handler for every route.
func (s *Server) Handler() http.Handler { return s.handler }

// Start builds the user token verifier when an auth provider is configured.
// The verifier refreshes its keys in the background until Stop.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		return errors.New("api: already started")
	}

	// The key refresh must outlive the start-up context, so it gets its own,
	// cancelled by Stop.
	refreshCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	if err := s.users.start(ctx, refreshCtx, s.log); err != nil {
		cancel()

		return err
	}

	s.cancel = cancel

	return nil
}

// Stop ends the background key refresh.
func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}

	return nil
}

// routes builds the mux. Unknown paths go to the web UI, which answers
// unknown /api/ paths with a JSON 404; a known path with another method is
// answered 405. Below /api/agent/ both are answered in the agent protocol's
// error shape and counted like every other agent request.
func (s *Server) routes(ui http.Handler) http.Handler {
	mux := http.NewServeMux()
	methods := make(map[string][]string, 32)

	handle := func(method, pattern string, handler http.Handler) {
		mux.Handle(method+" "+pattern, handler)

		methods[pattern] = append(methods[pattern], method)
	}

	handle(http.MethodPost, protocol.PathSync, http.HandlerFunc(s.handleSync))
	handle(http.MethodPost, protocol.PathInventory, http.HandlerFunc(s.handleInventory))

	handle(http.MethodGet, "/api/v1/config", http.HandlerFunc(s.handleConfig))
	handle(http.MethodGet, "/api/v1/public/hosts", http.HandlerFunc(s.handlePublicHosts))
	handle(http.MethodGet, "/api/v1/public/images", http.HandlerFunc(s.handlePublicImages))
	handle(http.MethodGet, "/api/v1/public/network", http.HandlerFunc(s.handlePublicNetwork))
	handle(http.MethodGet, "/api/v1/public/history", http.HandlerFunc(s.handlePublicHistory))

	// A userHandler can only be registered through here, behind the user
	// authentication.
	user := func(method, pattern string, handler userHandler) {
		handle(method, pattern, s.user(handler))

		s.userRoutes = append(s.userRoutes, method+" "+pattern)
	}

	user(http.MethodGet, "/api/v1/hosts", s.handleHosts)
	user(http.MethodGet, "/api/v1/hosts/{name}", s.handleHost)
	user(http.MethodGet, "/api/v1/packages", s.handlePackages)
	user(http.MethodGet, "/api/v1/rollouts", s.handleRollouts)
	user(http.MethodGet, "/api/v1/plan", s.handlePlan)
	user(http.MethodGet, "/api/v1/settings", s.handleGetSettings)
	user(http.MethodPut, "/api/v1/settings", s.handlePutSettings)
	user(http.MethodGet, "/api/v1/audit", s.handleAudit)

	for name, handler := range s.actions() {
		user(http.MethodPost, "/api/v1/actions/"+name, handler)
	}

	handle(http.MethodGet, "/healthz", http.HandlerFunc(s.handleHealthz))
	handle(http.MethodGet, "/metrics", promhttp.HandlerFor(s.opts.Registry, promhttp.HandlerOpts{
		ErrorLog:            s.log,
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: 4,
		Timeout:             20 * time.Second,
	}))

	// The catch-all takes every method, so the mux never answers 405 by
	// itself. known holds the same paths without methods to tell "wrong
	// method" from "no such path".
	known := http.NewServeMux()

	agentEndpoints := map[string]string{
		protocol.PathSync:      endpointSync,
		protocol.PathInventory: endpointInventory,
	}

	for pattern, allowed := range methods {
		allow := strings.Join(allowed, ", ")
		endpoint, agent := agentEndpoints[pattern]

		known.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)

			if agent {
				s.agentFailure(w, r, endpoint,
					agentErrorf(http.StatusMethodNotAllowed, "method not allowed"))

				return
			}

			s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		})
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if handler, pattern := known.Handler(r); pattern != "" {
			handler.ServeHTTP(w, r)

			return
		}

		if strings.HasPrefix(r.URL.Path, agentPathPrefix) {
			s.agentFailure(w, r, endpointUnknown,
				agentErrorf(http.StatusNotFound, "not found"))

			return
		}

		ui.ServeHTTP(w, r)
	})

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
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON writes v as the JSON answer.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.log.WithError(err).Error("Cannot encode response")

		status = http.StatusInternalServerError
		body = []byte(`{"error":"internal error"}`)
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(status)

	if _, err := w.Write(append(body, '\n')); err != nil {
		s.log.WithError(err).Debug("Cannot write response")
	}
}

// writeError writes an ErrorBody.
func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, ErrorBody{Error: msg})
}

// internalError logs err and answers 500 without detail: store and scheduler
// errors can carry anything.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		// The client went away; nobody reads the answer.
		s.writeError(w, http.StatusServiceUnavailable, "request cancelled")

		return
	}

	s.log.WithError(err).WithFields(logrus.Fields{
		"method": r.Method,
		"path":   r.URL.Path,
	}).Error("Request failed")
	s.writeError(w, http.StatusInternalServerError, "internal error")
}

// remoteAddr returns the client address without port. With
// TrustForwardedHeaders it is the last entry of X-Forwarded-For, which is
// the one the nearest proxy added; earlier entries are client-supplied.
func (s *Server) remoteAddr(r *http.Request) string {
	if s.opts.TrustForwardedHeaders {
		values := r.Header.Values("X-Forwarded-For")
		if len(values) > 0 {
			parts := strings.Split(values[len(values)-1], ",")
			candidate := strings.TrimSpace(parts[len(parts)-1])

			if ip := net.ParseIP(candidate); ip != nil {
				return ip.String()
			}
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}
