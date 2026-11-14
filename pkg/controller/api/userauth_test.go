package api

import (
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/issuer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

const (
	// discoveredIssuer differs from the provider URL, which shows that the
	// discovery document is used.
	discoveredIssuer = "https://auth.testnet.example"
	tokenScope       = "*.testnet.example"
	userEmail        = "alice@ethpandaops.io"
)

// authProvider is a stand-in authenticatoor: discovery document and JWKS.
type authProvider struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	discovery atomic.Bool
	clock     *clock
}

func newAuthProvider(t *testing.T, c *clock, discovery bool) *authProvider {
	t.Helper()

	key, err := issuer.GenerateRSAKey()
	require.NoError(t, err)

	p := &authProvider{key: key, clock: c}
	p.discovery.Store(discovery)

	jwks, err := p.issuer(t, nil).JWKS()
	require.NoError(t, err)

	serveJWKS := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	}

	mux := http.NewServeMux()
	// The discovery document points at a non-default JWKS path.
	mux.HandleFunc("/keys", serveJWKS)
	mux.HandleFunc("/jwks.json", serveJWKS)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if !p.discovery.Load() {
			http.NotFound(w, nil)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": discoveredIssuer, "jwks_uri": p.srv.URL + "/keys",
		})
	})

	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)

	return p
}

// issuerName is the "iss" the controller expects.
func (p *authProvider) issuerName() string {
	if p.discovery.Load() {
		return discoveredIssuer
	}

	return p.srv.URL
}

// issuer returns a token issuer; mod can break its configuration.
func (p *authProvider) issuer(t *testing.T, mod func(*issuer.Config)) *issuer.RS256Issuer {
	t.Helper()

	cfg := issuer.Config{
		ActiveKey: p.key,
		TTL:       30 * time.Minute,
		Scope:     tokenScope,
		Now:       p.clock.Now,
	}

	if p.srv != nil {
		cfg.Issuer = p.issuerName()
		cfg.DefaultAudience = []string{parentZone(p.srv.URL)}
	} else {
		cfg.Issuer = discoveredIssuer
	}

	if mod != nil {
		mod(&cfg)
	}

	iss, err := issuer.NewRS256Issuer(cfg)
	require.NoError(t, err)

	return iss
}

// token mints a token for subject and email.
func (p *authProvider) token(t *testing.T, subject, email string, mod func(*issuer.Config)) string {
	t.Helper()

	tok, _, err := p.issuer(t, mod).Issue(subject, email, nil)
	require.NoError(t, err)

	return tok
}

// authHarness is a started Server with an auth provider.
func authHarness(t *testing.T, discovery bool) (*harness, *authProvider) {
	t.Helper()

	c := &clock{now: testEpoch}
	provider := newAuthProvider(t, c, discovery)

	h := newHarness(t, func(o *Options) {
		o.AuthProviderURL = provider.srv.URL + "/"
		o.Now = c.Now
	})
	h.clock = c
	h.start()

	return h, provider
}

// userRoute is one authenticated route with a request that succeeds.
type userRoute struct {
	method string
	path   string
	body   string
}

func userRoutes() []userRoute {
	settings, err := json.Marshal(fleet.DefaultSettings())
	if err != nil {
		panic(err)
	}

	return []userRoute{
		{method: http.MethodGet, path: "/api/v1/hosts"},
		{method: http.MethodGet, path: "/api/v1/hosts/lighthouse-geth-1"},
		{method: http.MethodGet, path: "/api/v1/packages?q=openssl"},
		{method: http.MethodGet, path: "/api/v1/rollouts"},
		{method: http.MethodGet, path: "/api/v1/plan"},
		{method: http.MethodGet, path: "/api/v1/settings"},
		{method: http.MethodPut, path: "/api/v1/settings", body: string(settings)},
		{method: http.MethodGet, path: "/api/v1/audit"},
		{method: http.MethodPost, path: "/api/v1/actions/roll", body: `{"all":true}`},
		{method: http.MethodPost, path: "/api/v1/actions/reboot", body: `{"hosts":["a"]}`},
		{method: http.MethodPost, path: "/api/v1/actions/hold", body: `{"hosts":["a"]}`},
		{method: http.MethodPost, path: "/api/v1/actions/unhold", body: `{"hosts":["a"]}`},
		{method: http.MethodPost, path: "/api/v1/actions/pause", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/actions/resume", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/actions/skip", body: `{"rollout":"r","hosts":["a"]}`},
		{method: http.MethodPost, path: "/api/v1/actions/retry", body: `{"rollout":"r"}`},
		{method: http.MethodPost, path: "/api/v1/actions/abort", body: `{"rollout":"r"}`},
		{method: http.MethodPost, path: "/api/v1/actions/forget", body: `{"hosts":["a"]}`},
		{method: http.MethodPost, path: "/api/v1/actions/end-lease", body: `{"lease":"l"}`},
	}
}

// request builds the route's request; authorization is the full header value.
func (r userRoute) request(authorization string) *http.Request {
	req := httptest.NewRequest(r.method, testBaseURL+r.path, strings.NewReader(r.body))

	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	return req
}

func seedHost(h *harness) {
	h.store.hosts["lighthouse-geth-1"] = &fleet.Host{Name: "lighthouse-geth-1", LastSeen: testEpoch}
}

func TestUserRoutesNeedToken(t *testing.T) {
	h, provider := authHarness(t, true)
	seedHost(h)

	valid := provider.token(t, "alice", userEmail, nil)

	for _, route := range userRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			before := len(h.sched.recorded())

			for name, authorization := range map[string]string{
				"no token":      "",
				"garbage token": "Bearer not.a.jwt",
				"basic auth":    "Basic YWxpY2U6c2VjcmV0",
				"bare token":    valid,
				"empty bearer":  "Bearer ",
			} {
				rec := h.serve(route.request(authorization))
				require.Equal(t, http.StatusUnauthorized, rec.Code, name)
				assert.Equal(t, `Bearer realm="upgradoor"`, rec.Header().Get("WWW-Authenticate"))
				assert.Equal(t, ErrorBody{Error: errUnauthenticated.Error()},
					decodeJSON[ErrorBody](t, rec), name)
			}

			// A token in the URL is not a token.
			sep := "?"
			if strings.Contains(route.path, "?") {
				sep = "&"
			}

			inURL := route
			inURL.path += sep + "token=" + valid + "&access_token=" + valid
			require.Equal(t, http.StatusUnauthorized, h.serve(inURL.request("")).Code)

			assert.Len(t, h.sched.recorded(), before, "nothing reached the scheduler")

			rec := h.serve(route.request("Bearer " + valid))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			if route.method != http.MethodGet {
				calls := h.sched.recorded()
				require.Len(t, calls, before+1)
				assert.Equal(t, userEmail, calls[before].Actor)
			}
		})
	}
}

func TestUserTokens(t *testing.T) {
	h, provider := authHarness(t, true)

	otherKey, err := issuer.GenerateRSAKey()
	require.NoError(t, err)

	tests := []struct {
		name      string
		subject   string
		email     string
		mod       func(*issuer.Config)
		host      string
		scheme    string
		status    int
		wantActor string
	}{
		{
			name: "valid", subject: "alice", email: userEmail,
			status: http.StatusOK, wantActor: userEmail,
		},
		{
			name: "valid, subject when there is no email", subject: "github|alice",
			status: http.StatusOK, wantActor: "github|alice",
		},
		{
			name: "valid, lower-case scheme", subject: "alice", email: userEmail,
			scheme: "bearer", status: http.StatusOK, wantActor: userEmail,
		},
		{
			name: "valid, host with port", subject: "alice", email: userEmail,
			host: testHost + ":8443", status: http.StatusOK, wantActor: userEmail,
		},
		{
			name: "expired", subject: "alice", email: userEmail,
			mod: func(c *issuer.Config) {
				c.Now = func() time.Time { return testEpoch.Add(-2 * time.Hour) }
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "wrong audience", subject: "alice", email: userEmail,
			mod:    func(c *issuer.Config) { c.DefaultAudience = []string{"other.example"} },
			status: http.StatusUnauthorized,
		},
		{
			name: "wrong host scope", subject: "alice", email: userEmail,
			mod:    func(c *issuer.Config) { c.Scope = "*.other-devnet.example" },
			status: http.StatusUnauthorized,
		},
		{
			name: "scope of this devnet, request for another host", subject: "alice",
			email: userEmail, host: "upgradoor.other-devnet.example",
			status: http.StatusUnauthorized,
		},
		{
			name: "no scope at all", subject: "alice", email: userEmail,
			mod:    func(c *issuer.Config) { c.Scope = "" },
			status: http.StatusUnauthorized,
		},
		{
			name: "wrong issuer", subject: "alice", email: userEmail,
			mod:    func(c *issuer.Config) { c.Issuer = "https://auth.evil.example" },
			status: http.StatusUnauthorized,
		},
		{
			name: "signed by another key", subject: "alice", email: userEmail,
			mod:    func(c *issuer.Config) { c.ActiveKey = otherKey },
			status: http.StatusUnauthorized,
		},
		{
			name: "another key under the provider's key id", subject: "alice", email: userEmail,
			mod: func(c *issuer.Config) {
				c.ActiveKeyID = issuer.DeriveKeyID(&provider.key.PublicKey)
				c.ActiveKey = otherKey
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "service denied by the token", subject: "alice", email: userEmail,
			mod:    func(c *issuer.Config) { c.Services = "+all,-upgradoor" },
			status: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := tt.scheme
			if scheme == "" {
				scheme = "Bearer"
			}

			route := userRoute{method: http.MethodPost, path: "/api/v1/actions/pause", body: "{}"}
			req := route.request(scheme + " " + provider.token(t, tt.subject, tt.email, tt.mod))

			if tt.host != "" {
				req.Host = tt.host
			}

			before := len(h.sched.recorded())
			rec := h.serve(req)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())

			calls := h.sched.recorded()

			if tt.status != http.StatusOK {
				assert.Len(t, calls, before)
				// The reason stays in the log; the answer does not say which
				// check failed.
				assert.Equal(t, ErrorBody{Error: errUnauthenticated.Error()},
					decodeJSON[ErrorBody](t, rec))

				return
			}

			require.Len(t, calls, before+1)
			assert.Equal(t, tt.wantActor, calls[before].Actor)
		})
	}
}

func TestUserTokenExpiresWithTheClock(t *testing.T) {
	h, provider := authHarness(t, true)

	route := userRoute{method: http.MethodGet, path: "/api/v1/plan"}
	authorization := "Bearer " + provider.token(t, "alice", userEmail, nil)

	require.Equal(t, http.StatusOK, h.serve(route.request(authorization)).Code)

	h.clock.Advance(31 * time.Minute)

	require.Equal(t, http.StatusUnauthorized, h.serve(route.request(authorization)).Code)
}

func TestUserAuthDiscoveryFallback(t *testing.T) {
	// Without a discovery document the issuer is the provider URL and the
	// keys are at <url>/jwks.json.
	h, provider := authHarness(t, false)

	route := userRoute{method: http.MethodGet, path: "/api/v1/plan"}

	rec := h.serve(route.request("Bearer " + provider.token(t, "alice", userEmail, nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A token naming the issuer a discovery document would have given is
	// not accepted here.
	wrong := provider.token(t, "alice", userEmail, func(c *issuer.Config) {
		c.Issuer = discoveredIssuer
	})
	require.Equal(t, http.StatusUnauthorized, h.serve(route.request("Bearer "+wrong)).Code)
}

func TestUserRoutesBeforeStart(t *testing.T) {
	c := &clock{now: testEpoch}
	provider := newAuthProvider(t, c, true)

	h := newHarness(t, func(o *Options) {
		o.AuthProviderURL = provider.srv.URL
		o.Now = c.Now
	})

	authorization := "Bearer " + provider.token(t, "alice", userEmail, nil)

	for _, route := range userRoutes() {
		rec := h.serve(route.request(authorization))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, route.path)
		assert.Equal(t, "5", rec.Header().Get("Retry-After"))
	}

	assert.Empty(t, h.sched.recorded())

	// Public and agent routes do not wait for the auth provider.
	assert.Equal(t, http.StatusOK, h.get("/api/v1/public/hosts").Code)
	assert.Equal(t, http.StatusOK, h.get("/healthz").Code)

	h.start()

	rec := h.serve(userRoute{method: http.MethodGet, path: "/api/v1/plan"}.request(authorization))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestOpenMode(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	seedHost(h)

	for _, route := range userRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			before := len(h.sched.recorded())

			// No token, and a token nobody checks.
			for _, authorization := range []string{"", "Bearer whatever"} {
				rec := h.serve(route.request(authorization))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}

			if route.method != http.MethodGet {
				calls := h.sched.recorded()
				require.Len(t, calls, before+2)
				assert.Equal(t, actorAnonymous, calls[before].Actor)
			}
		})
	}
}

func TestStartStop(t *testing.T) {
	h := newHarness(t, nil)

	require.NoError(t, h.srv.Stop(), "stop before start is harmless")
	require.NoError(t, h.srv.Start(t.Context()))
	require.Error(t, h.srv.Start(t.Context()), "a second start is refused")
	require.NoError(t, h.srv.Stop())
	require.NoError(t, h.srv.Stop())
}

func TestParentZoneAndStripPort(t *testing.T) {
	zones := map[string]string{
		"https://auth.devnet-0.ethpandaops.io":      "devnet-0.ethpandaops.io",
		"https://auth.devnet-0.ethpandaops.io/":     "devnet-0.ethpandaops.io",
		"https://auth.devnet-0.ethpandaops.io:8443": "devnet-0.ethpandaops.io",
		"http://auth.local/path":                    "local",
		"http://localhost:8080":                     "localhost",
	}

	for in, want := range zones {
		assert.Equal(t, want, parentZone(in), in)
	}

	hosts := map[string]string{
		"":                       "",
		"upgradoor.example":      "upgradoor.example",
		"upgradoor.example:8443": "upgradoor.example",
		"[2001:db8::1]:8443":     "2001:db8::1",
		"[2001:db8::1]":          "2001:db8::1",
		"[broken":                "[broken",
	}

	for in, want := range hosts {
		assert.Equal(t, want, stripPort(in), in)
	}
}
