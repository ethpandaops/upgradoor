// Package agentapi is the controller's side of the agent protocol: the sync
// and inventory endpoints, the verification of the agents' certificates and
// request proofs, and the pacing of their syncs.
package agentapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/metrics"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Names that several encodings share: a log field and keys of audit details,
// which the event stream's encoding and the UI read.
const (
	keyHost  = "host"
	keyAfter = "after"
)

// Store is the part of store.Store the agent API needs.
type Store interface {
	RecordSync(ctx context.Context, sync *store.HostSync) (*store.SyncResult, error)
	GetHost(ctx context.Context, name string) (*fleet.Host, error)
	SaveInventory(ctx context.Context, host string, inv *protocol.Inventory) error
	AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error
}

// Options configures the agent API.
type Options struct {
	// Verifier authenticates agent requests.
	Verifier *identity.Verifier
	// PublicURLs are the base URLs under which agents reach the controller,
	// as NormalizePublicURLs returns them. A request proof must name one of
	// them. When empty the base is derived from the request's Host header:
	// "https://<host>", and "http://<host>" only with AllowInsecureAgentHTTP.
	PublicURLs []string
	// AllowInsecureAgentHTTP accepts proofs for plain http URLs.
	AllowInsecureAgentHTTP bool
	// AnswerSigner signs every answer; nil leaves them unsigned.
	AnswerSigner *identity.AnswerSigner
	// TrustForwardedHeaders takes the remote address from X-Forwarded-For.
	TrustForwardedHeaders bool

	Store     Store
	Scheduler scheduler.Service
	// Lookups says what the tags of the hosts point at.
	Lookups discovery.Tracker
	Metrics metrics.Metrics
	// Notify says that a sync or an inventory changed what the views show.
	Notify func()
	// NotifyHost, when set, says that a sync stored new probe values of the
	// host (its details) and nothing else.
	NotifyHost func(host string)
	// Now is the clock.
	Now func() time.Time
}

// Handler serves the agent endpoints.
type Handler struct {
	log  logrus.FieldLogger
	opts Options
	now  func() time.Time

	publicURLs []string
	metrics    metrics.Metrics

	syncSlots            chan struct{}
	pacer                *pacer
	lookups              discovery.Tracker
	inventorySlots       chan struct{}
	syncReadTimeout      time.Duration
	inventoryReadTimeout time.Duration

	// overlapAudited remembers, per host, the hosts its reported validator
	// range last overlapped with: an overlap is audited once per episode.
	// validatorsAudited limits the audit entries about one host's range. Both
	// are guarded by auditMu.
	auditMu           sync.Mutex
	overlapAudited    map[string]string
	validatorsAudited map[string]time.Time
}

// New returns the agent API over opts.
//
//nolint:gocritic // hugeParam: Options by value, like the other handlers of the controller.
func New(log logrus.FieldLogger, opts Options) *Handler {
	return &Handler{
		log:            log,
		opts:           opts,
		now:            opts.Now,
		publicURLs:     opts.PublicURLs,
		metrics:        opts.Metrics,
		syncSlots:      make(chan struct{}, maxConcurrentSyncs),
		pacer:          newPacer(),
		lookups:        opts.Lookups,
		inventorySlots: make(chan struct{}, maxConcurrentInventories),

		syncReadTimeout:      syncReadTimeout,
		inventoryReadTimeout: inventoryReadTimeout,
	}
}

// Route is one route of the agent API.
type Route struct {
	Method  string
	Pattern string
	Handler http.HandlerFunc
}

// Routes are the agent endpoints.
func (s *Handler) Routes() []Route {
	return []Route{
		{Method: http.MethodPost, Pattern: protocol.PathSync, Handler: s.handleSync},
		{Method: http.MethodPost, Pattern: protocol.PathInventory, Handler: s.handleInventory},
	}
}

// remoteAddr returns the client address without port. With
// TrustForwardedHeaders it is the last entry of X-Forwarded-For, which is
// the one the nearest proxy added; earlier entries are client-supplied.
func (s *Handler) remoteAddr(r *http.Request) string {
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

// NormalizePublicURLs checks the configured base URLs and strips trailing
// slashes.
func NormalizePublicURLs(raw []string, allowHTTP bool) ([]string, error) {
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
