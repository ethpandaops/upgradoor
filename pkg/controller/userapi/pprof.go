package userapi

import (
	"context"
	"errors"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // on http.DefaultServeMux, which only Pprof serves, behind the admin gate
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
)

const (
	// PprofPath is where the profiles are served.
	PprofPath = "/debug/pprof"
	// PprofTokenCookie carries the login token for the profiles in a
	// browser, set by the gate page (web/src/components/PprofGate.tsx).
	PprofTokenCookie = "upgradoor_debug_token" //nolint:gosec // a cookie name, no credential
	// AuditActionPprof records a profile download.
	AuditActionPprof = "debug.pprof"

	pprofAuditTimeout = 5 * time.Second
	maxPprofQuery     = 256
)

// Pprof serves the profiles to a login with the feature admin. The token
// comes as a bearer header (curl, go tool pprof) or as PprofTokenCookie (a
// browser). A browser without a valid token gets gate, which sets the
// cookie from the session; anything else is a 401. A login without admin is
// refused and audited like any refusal. Every profile download is audited;
// the index page is not.
func (s *Handler) Pprof(gate http.Handler) http.Handler {
	// net/http/pprof registers on http.DefaultServeMux. It is safe to serve
	// here: no server of the controller serves that mux, so the profiles are
	// only reachable through this gate (TestNoServerServesDefaultServeMux).
	profiles := http.DefaultServeMux

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			if c, err := r.Cookie(PprofTokenCookie); err == nil {
				token = c.Value
			}
		}

		u, err := s.users.authenticateToken(r, token)

		switch {
		case err == nil && u.Open:
			// Validation keeps pprof off without an auth provider.
			httpjson.Error(s.log, w, http.StatusNotFound, "not found")

			return
		case errors.Is(err, errAuthNotReady):
			w.Header().Set("Retry-After", "5")
			httpjson.Error(s.log, w, http.StatusServiceUnavailable, errAuthNotReady.Error())

			return
		case err != nil && r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html"):
			w.Header().Set("Cache-Control", "no-store")
			gate.ServeHTTP(w, r)

			return
		case err != nil:
			Unauthorized(s.log, w)

			return
		}

		if _, ok := s.authz.Allow(w, r, u, &authz.Requirement{Feature: authz.FeatureAdmin}); !ok {
			return
		}

		w.Header().Set("Cache-Control", "no-store")

		profile := strings.Trim(strings.TrimPrefix(r.URL.Path, PprofPath), "/")
		if profile == "" {
			profiles.ServeHTTP(w, r)

			return
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		profiles.ServeHTTP(rec, r)

		s.auditPprof(r, u, profile, rec.status)
	})
}

// auditPprof records a profile download.
func (s *Handler) auditPprof(r *http.Request, u *authz.User, profile string, status int) {
	query := r.URL.RawQuery
	if len(query) > maxPprofQuery {
		query = query[:maxPprofQuery]
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), pprofAuditTimeout)
	defer cancel()

	if err := s.opts.Store.AppendAudit(ctx, fleet.AuditEntry{
		Actor: u.Actor, Action: AuditActionPprof, Target: authz.Echo(r.URL.Path),
		Detail: map[string]any{
			"profile": authz.Echo(profile), "query": authz.Echo(query), "status": status,
		},
	}); err != nil {
		s.log.WithError(err).WithField("profile", authz.Echo(profile)).Warn("Could not audit a profile download")
	}
}

// statusRecorder keeps the status a handler answered with.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

var _ http.Flusher = (*statusRecorder)(nil)

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush keeps the streamed profiles (CPU, trace) flushable.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the writer, e.g. for the write
// deadline of a long profile.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
