package userapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/issuer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
)

// userRoute is one authenticated route with a request that succeeds.
type userRoute struct {
	method string
	path   string
	body   string
}

func userRoutes() []userRoute {
	// A body that changes a setting of the defaults: one that changes nothing
	// does not reach the scheduler.
	settings := []byte(`{"minPeers":7}`)

	return []userRoute{
		{method: http.MethodGet, path: "/api/v1/hosts"},
		{method: http.MethodGet, path: "/api/v1/hosts/lighthouse-geth-1"},
		{method: http.MethodGet, path: "/api/v1/packages?q=openssl"},
		{method: http.MethodGet, path: "/api/v1/rollouts"},
		{method: http.MethodGet, path: "/api/v1/plan"},
		{method: http.MethodGet, path: "/api/v1/settings"},
		{method: http.MethodPut, path: "/api/v1/settings", body: string(settings)},
		{method: http.MethodGet, path: "/api/v1/audit"},
		{method: http.MethodGet, path: "/api/v1/slots"},
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
	req := httptest.NewRequest(r.method, controllertest.BaseURL+r.path, strings.NewReader(r.body))

	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	return req
}

func seedHost(h *harness) {
	h.store.Hosts["lighthouse-geth-1"] = &fleet.Host{Name: "lighthouse-geth-1", LastSeen: controllertest.Epoch}
}

func TestUserRoutesNeedToken(t *testing.T) {
	h, provider := authHarness(t, true)
	seedHost(h)

	valid := provider.Token(t, "alice", controllertest.UserEmail, nil)

	for _, route := range userRoutes() {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			before := len(h.sched.Recorded())

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
				assert.Equal(t, httpjson.ErrorBody{Error: errUnauthenticated.Error()},
					controllertest.DecodeJSON[httpjson.ErrorBody](t, rec), name)
			}

			// A token in the URL is not a token.
			sep := "?"
			if strings.Contains(route.path, "?") {
				sep = "&"
			}

			inURL := route
			inURL.path += sep + "token=" + valid + "&access_token=" + valid
			require.Equal(t, http.StatusUnauthorized, h.serve(inURL.request("")).Code)

			assert.Len(t, h.sched.Recorded(), before, "nothing reached the scheduler")

			rec := h.serve(route.request("Bearer " + valid))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			if route.method != http.MethodGet {
				calls := h.sched.Recorded()
				require.Len(t, calls, before+1)
				assert.Equal(t, controllertest.UserEmail, calls[before].Actor)
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
			name: "valid", subject: "alice", email: controllertest.UserEmail,
			status: http.StatusOK, wantActor: controllertest.UserEmail,
		},
		{
			name: "valid, subject when there is no email", subject: "github|alice",
			status: http.StatusOK, wantActor: "github|alice",
		},
		{
			name: "valid, lower-case scheme", subject: "alice", email: controllertest.UserEmail,
			scheme: "bearer", status: http.StatusOK, wantActor: controllertest.UserEmail,
		},
		{
			name: "valid, host with port", subject: "alice", email: controllertest.UserEmail,
			host: controllertest.ControllerHost + ":8443", status: http.StatusOK, wantActor: controllertest.UserEmail,
		},
		{
			name: "expired", subject: "alice", email: controllertest.UserEmail,
			mod: func(c *issuer.Config) {
				c.Now = func() time.Time { return controllertest.Epoch.Add(-2 * time.Hour) }
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "wrong audience", subject: "alice", email: controllertest.UserEmail,
			mod:    func(c *issuer.Config) { c.DefaultAudience = []string{"other.example"} },
			status: http.StatusUnauthorized,
		},
		{
			name: "wrong host scope", subject: "alice", email: controllertest.UserEmail,
			mod:    func(c *issuer.Config) { c.Scope = "*.other-devnet.example" },
			status: http.StatusUnauthorized,
		},
		{
			name: "scope of this devnet, request for another host", subject: "alice",
			email: controllertest.UserEmail, host: "upgradoor.other-devnet.example",
			status: http.StatusUnauthorized,
		},
		{
			name: "no scope at all", subject: "alice", email: controllertest.UserEmail,
			mod:    func(c *issuer.Config) { c.Scope = "" },
			status: http.StatusUnauthorized,
		},
		{
			name: "wrong issuer", subject: "alice", email: controllertest.UserEmail,
			mod:    func(c *issuer.Config) { c.Issuer = "https://auth.evil.example" },
			status: http.StatusUnauthorized,
		},
		{
			name: "signed by another key", subject: "alice", email: controllertest.UserEmail,
			mod:    func(c *issuer.Config) { c.ActiveKey = otherKey },
			status: http.StatusUnauthorized,
		},
		{
			name: "another key under the provider's key id", subject: "alice", email: controllertest.UserEmail,
			mod: func(c *issuer.Config) {
				c.ActiveKeyID = issuer.DeriveKeyID(&provider.Key.PublicKey)
				c.ActiveKey = otherKey
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "service denied by the token", subject: "alice", email: controllertest.UserEmail,
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
			req := route.request(scheme + " " + provider.Token(t, tt.subject, tt.email, tt.mod))

			if tt.host != "" {
				req.Host = tt.host
			}

			before := len(h.sched.Recorded())
			rec := h.serve(req)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())

			calls := h.sched.Recorded()

			if tt.status != http.StatusOK {
				assert.Len(t, calls, before)
				// The reason stays in the log; the answer does not say which
				// check failed.
				assert.Equal(t, httpjson.ErrorBody{Error: errUnauthenticated.Error()},
					controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))

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
	authorization := "Bearer " + provider.Token(t, "alice", controllertest.UserEmail, nil)

	require.Equal(t, http.StatusOK, h.serve(route.request(authorization)).Code)

	h.clock.Advance(31 * time.Minute)

	require.Equal(t, http.StatusUnauthorized, h.serve(route.request(authorization)).Code)
}

func TestUserAuthDiscoveryFallback(t *testing.T) {
	// Without a discovery document the issuer is the provider URL and the
	// keys are at <url>/jwks.json.
	h, provider := authHarness(t, false)

	route := userRoute{method: http.MethodGet, path: "/api/v1/plan"}

	rec := h.serve(route.request("Bearer " + provider.Token(t, "alice", controllertest.UserEmail, nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A token naming the issuer a discovery document would have given is
	// not accepted here.
	wrong := provider.Token(t, "alice", controllertest.UserEmail, func(c *issuer.Config) {
		c.Issuer = controllertest.DiscoveredIssuer
	})
	require.Equal(t, http.StatusUnauthorized, h.serve(route.request("Bearer "+wrong)).Code)
}

func TestUserRoutesBeforeStart(t *testing.T) {
	c := controllertest.NewClock(controllertest.Epoch)
	provider := controllertest.NewAuthProvider(t, c, true)

	h := newHarness(t, func(o *Options) {
		o.AuthProviderURL = provider.Server.URL
		o.Now = c.Now
	})

	authorization := "Bearer " + provider.Token(t, "alice", controllertest.UserEmail, nil)

	for _, route := range userRoutes() {
		rec := h.serve(route.request(authorization))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, route.path)
		assert.Equal(t, "5", rec.Header().Get("Retry-After"))
	}

	assert.Empty(t, h.sched.Recorded())

	// Public routes do not wait for the auth provider (nor does the health
	// check: the server's TestHealthz).
	assert.Equal(t, http.StatusOK, h.get("/api/v1/public/hosts").Code)

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
			before := len(h.sched.Recorded())

			// No token, and a token nobody checks.
			for _, authorization := range []string{"", "Bearer whatever"} {
				// The same settings body twice is a change only the first time.
				h.sched.Set(func(f *controllertest.Scheduler) { f.CurrentSettings = fleet.DefaultSettings() })

				rec := h.serve(route.request(authorization))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}

			if route.method != http.MethodGet {
				calls := h.sched.Recorded()
				require.Len(t, calls, before+2)
				assert.Equal(t, actorAnonymous, calls[before].Actor)
			}
		})
	}
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

// TestTokenClaimsAuthorize: the features and teams the handlers decide with
// are the ones of the verified token.
func TestTokenClaimsAuthorize(t *testing.T) {
	h, provider := authHarness(t, true)
	controllertest.OwnershipFixture(h.store)

	post := func(action, body, authorization string) *httptest.ResponseRecorder {
		return h.serve(userRoute{
			method: http.MethodPost, path: "/api/v1/actions/" + action, body: body,
		}.request(authorization))
	}

	holdMixed := `{"hosts":["` + controllertest.HostMixed + `"],"reason":"debugging"}`

	tests := []struct {
		name          string
		action        string
		body          string
		authorization string
		by            string
		teams         []string
		refusal       string
	}{
		{
			name: "team twin, owning team", action: "hold", body: holdMixed,
			authorization: provider.Grant(t, "-all,+upgradoor,+upgradoor:team_*", "lighthouse"),
			by:            "team_hold", teams: []string{"lighthouse"},
		},
		{
			name: "team twin, several teams", action: "hold", body: holdMixed,
			authorization: provider.Grant(t, "+upgradoor:team_hold", "teku", "geth"),
			by:            "team_hold", teams: []string{"geth", "teku"},
		},
		{
			name: "team twin, another team", action: "hold", body: holdMixed,
			authorization: provider.Grant(t, "+upgradoor:team_hold", "teku"),
			refusal: "forbidden: upgradoor:team_hold does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "team twin, a token without teams", action: "hold", body: holdMixed,
			authorization: provider.Grant(t, "+upgradoor:team_hold"),
			refusal: "forbidden: upgradoor:team_hold does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "roll of mixed ownership", action: "roll",
			body:          `{"hosts":["` + controllertest.HostMixed + `"],"containers":["lighthouse","geth"]}`,
			authorization: provider.Grant(t, "+upgradoor:team_roll", "lighthouse"),
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "another feature", action: "reboot", body: `{"hosts":["` + controllertest.HostMixed + `"]}`,
			authorization: provider.Grant(t, "+upgradoor:hold:roll", "lighthouse"),
			refusal:       "forbidden: this login lacks the feature upgradoor:reboot",
		},
		{
			name: "plain feature", action: "reboot", body: `{"hosts":["` + controllertest.HostMixed + `"]}`,
			authorization: provider.Grant(t, "+upgradoor:reboot", "teku"),
			by:            "reboot", teams: []string{"teku"},
		},
		{
			name: "force without admin", action: "reboot",
			body:          `{"hosts":["` + controllertest.HostMixed + `"],"force":true}`,
			authorization: provider.Grant(t, "+upgradoor:reboot"),
			refusal:       "forbidden: this login lacks the feature upgradoor:admin (needed for force)",
		},
		{
			name: "staff", action: "reboot", body: `{"all":true,"force":true}`,
			authorization: provider.Grant(t, "+all:all", "panda"),
			by:            "reboot,admin", teams: []string{"panda"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := h.sched.Recorded()
			rec := post(tt.action, tt.body, tt.authorization)
			calls := h.sched.Recorded()

			if tt.refusal != "" {
				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.JSONEq(t, `{"error":`+string(controllertest.MustJSON(t, tt.refusal))+`}`, rec.Body.String())
				assert.Empty(t, rec.Header().Get("WWW-Authenticate"), "403 is not a login problem")
				assert.Len(t, calls, len(before))

				return
			}

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Len(t, calls, len(before)+1)
			assert.Equal(t, controllertest.UserEmail, calls[len(before)].Actor)
			assert.Equal(t, scheduler.Authorization{By: tt.by, Teams: tt.teams}, calls[len(before)].Auth)
		})
	}

	// A token that may not use the service at all is no login here.
	rec := post("hold", holdMixed, provider.Grant(t, "-all,+spamoor:all", "lighthouse"))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestConfiguredServiceName: with --auth-service the token is checked for
// that service, for access and for features.
func TestConfiguredServiceName(t *testing.T) {
	c := controllertest.NewClock(controllertest.Epoch)
	provider := controllertest.NewAuthProvider(t, c, true)

	h := newHarness(t, func(o *Options) {
		o.AuthProviderURL = provider.Server.URL
		o.AuthService = "upgradoor-dev"
		o.Now = c.Now
	})
	h.clock = c
	h.start()
	controllertest.OwnershipFixture(h.store)

	hold := userRoute{
		method: http.MethodPost, path: "/api/v1/actions/hold", body: `{"hosts":["` + controllertest.HostMixed + `"]}`,
	}
	read := userRoute{method: http.MethodGet, path: "/api/v1/hosts"}

	// Features of "upgradoor" are features of another service here.
	other := provider.Grant(t, "+upgradoor:all")
	require.Equal(t, http.StatusOK, h.serve(read.request(other)).Code)

	rec := h.serve(hold.request(other))
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, httpjson.ErrorBody{Error: "forbidden: this login lacks the feature upgradoor-dev:hold"},
		controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))

	require.Equal(t, http.StatusOK,
		h.serve(hold.request(provider.Grant(t, "-all,+upgradoor-dev:hold"))).Code)
	require.Equal(t, http.StatusUnauthorized,
		h.serve(read.request(provider.Grant(t, "-all,+upgradoor:all"))).Code)
}
