package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/auth"
	"github.com/sirupsen/logrus"
)

// DefaultAuthService is the name this binary answers to in the token's
// "services" claim unless Options.AuthService names another.
const DefaultAuthService = "upgradoor"

// authRealm is the realm of the WWW-Authenticate challenge.
const authRealm = "upgradoor"

// discoveryTimeout bounds the OIDC discovery request at start-up.
const discoveryTimeout = 10 * time.Second

var (
	// errAuthNotReady is returned while the token verifier is not built yet.
	errAuthNotReady = errors.New("authentication provider is not ready")
	// errUnauthenticated is returned for a missing or unacceptable token.
	errUnauthenticated = errors.New("a valid bearer token is required")
)

// userAuth authenticates people against the devnet's authenticatoor, the
// same way spamoor and buildoor do: bearer JWTs verified against the
// provider's JWKS, bound to the request host through the token's scope.
// Without a provider URL it is open.
type userAuth struct {
	providerURL string
	// service is the name of this service in the token's "services" claim.
	service string
	now     func() time.Time

	mu       sync.RWMutex
	verifier auth.Verifier
}

func newUserAuth(providerURL, service string, now func() time.Time) *userAuth {
	return &userAuth{providerURL: providerURL, service: service, now: now}
}

// user is who makes an authenticated request, and what the token says they
// may do.
type user struct {
	// actor is the audit name: the token's email, falling back to its
	// subject, or "anonymous" in open mode.
	actor string
	// open is true when no auth provider is configured: everything is
	// allowed.
	open bool
	// teams are the token's teams.
	teams []string
	// services is the token's "services" claim, the directive list the
	// feature checks read.
	services string
}

// open reports whether no provider is configured.
func (a *userAuth) open() bool { return a.providerURL == "" }

// start builds the verifier from the provider's discovery document, falling
// back to <url>/jwks.json with the URL as issuer. ctx bounds the discovery;
// refreshCtx is the lifetime of the background key refresh.
func (a *userAuth) start(ctx, refreshCtx context.Context, log logrus.FieldLogger) error {
	if a.open() {
		log.Warn("No auth provider configured: the API is open to everyone who can reach it")

		return nil
	}

	expectedIssuer := a.providerURL
	jwksURL := a.providerURL + "/jwks.json"

	discoveryCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	client := &http.Client{Timeout: discoveryTimeout}

	disc, err := auth.FetchDiscovery(discoveryCtx, client, a.providerURL)
	if err != nil {
		log.WithError(err).Warn("Auth provider discovery failed, using default JWKS location")
	} else {
		expectedIssuer = disc.Issuer
		jwksURL = disc.JWKSURI
	}

	verifier, err := auth.NewJWKSVerifier(refreshCtx, auth.VerifierConfig{
		JWKSURL:          jwksURL,
		ExpectedIssuer:   expectedIssuer,
		ExpectedAudience: parentZone(a.providerURL),
		ExpectedService:  a.service,
		Now:              a.now,
	})
	if err != nil {
		return fmt.Errorf("api: build token verifier: %w", err)
	}

	a.mu.Lock()
	a.verifier = verifier
	a.mu.Unlock()

	log.WithFields(logrus.Fields{
		"issuer":   expectedIssuer,
		"audience": parentZone(a.providerURL),
		"service":  a.service,
	}).Info("User authentication enabled")

	return nil
}

// authenticate returns who makes a request. The audit actor is the token's
// email, falling back to its subject, or "anonymous" in open mode. The token
// is taken from the Authorization header only, never from the URL, so it
// does not end up in access logs.
//
// A token that passes has basic access to this service (the verifier checks
// the "services" claim for it), which is all a read needs. What else it may
// do is decided per request by authorize.
func (a *userAuth) authenticate(r *http.Request) (*user, error) {
	if a.open() {
		return &user{actor: actorAnonymous, open: true}, nil
	}

	a.mu.RLock()
	verifier := a.verifier
	a.mu.RUnlock()

	if verifier == nil {
		return nil, errAuthNotReady
	}

	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)

	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil, errUnauthenticated
	}

	// An empty host would switch the verifier's host binding off.
	host := stripPort(r.Host)
	if host == "" {
		return nil, errUnauthenticated
	}

	claims, err := verifier.Verify(token, auth.WithRequestHost(host))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnauthenticated, err)
	}

	actor := claims.Email
	if actor == "" {
		actor = claims.Subject
	}

	if actor == "" {
		return nil, errUnauthenticated
	}

	// The teams decide and are written into audit entries: a token with an
	// unreasonable number of them is not a login.
	if len(claims.Teams) > maxTokenTeams {
		return nil, fmt.Errorf("%w: the token names %d teams, more than %d",
			errUnauthenticated, len(claims.Teams), maxTokenTeams)
	}

	return &user{actor: actor, teams: claims.Teams, services: claims.Services}, nil
}

// userHandler is a handler for an authenticated route; u is who asks.
type userHandler func(w http.ResponseWriter, r *http.Request, u *user)

// user guards a route with the user authentication.
func (s *Server) user(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.users.authenticate(r)

		switch {
		case err == nil:
			next(w, r, u)
		case errors.Is(err, errAuthNotReady):
			w.Header().Set("Retry-After", "5")
			s.writeError(w, http.StatusServiceUnavailable, errAuthNotReady.Error())
		default:
			s.log.WithError(err).WithField("path", r.URL.Path).Debug("Rejected user request")
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+authRealm+`"`)
			s.writeError(w, http.StatusUnauthorized, errUnauthenticated.Error())
		}
	}
}

// parentZone returns the parent DNS zone of a URL's host:
// "https://auth.foo.example" -> "foo.example". It is the expected token
// audience.
func parentZone(rawURL string) string {
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(rawURL, p) {
			rawURL = rawURL[len(p):]

			break
		}
	}

	if i := strings.IndexByte(rawURL, '/'); i >= 0 {
		rawURL = rawURL[:i]
	}

	if i := strings.IndexByte(rawURL, ':'); i >= 0 {
		rawURL = rawURL[:i]
	}

	if i := strings.IndexByte(rawURL, '.'); i > 0 {
		return rawURL[i+1:]
	}

	return rawURL
}

// stripPort removes a trailing ":port" from a Host header value, handling
// bracketed IPv6 hosts.
func stripPort(host string) string {
	if host == "" {
		return host
	}

	if host[0] == '[' {
		if i := strings.IndexByte(host, ']'); i >= 0 {
			return host[1:i]
		}

		return host
	}

	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		return host[:i]
	}

	return host
}
