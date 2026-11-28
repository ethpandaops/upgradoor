package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestNewValidatesOptions(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	signer, err := identity.DeriveSignerKey([]byte(controllertest.Seed))
	require.NoError(t, err)

	valid := func() Options {
		return Options{
			Network:         controllertest.Network,
			Verifier:        identity.NewVerifier(identity.PublicKey(signer), controllertest.Network),
			Store:           controllertest.NewStore(),
			Scheduler:       controllertest.NewScheduler(),
			Beacon:          &controllertest.Beacon{},
			DefaultSettings: fleet.DefaultSettings(),
		}
	}

	_, err = New(log, valid())
	require.NoError(t, err, "defaults for the clock and the registry")

	tests := []struct {
		name     string
		mod      func(*Options)
		contains string
	}{
		{name: "network", mod: func(o *Options) { o.Network = "" }, contains: "network"},
		{name: "verifier", mod: func(o *Options) { o.Verifier = nil }, contains: "verifier"},
		{name: "store", mod: func(o *Options) { o.Store = nil }, contains: "store"},
		{name: "scheduler", mod: func(o *Options) { o.Scheduler = nil }, contains: "scheduler"},
		{name: "beacon", mod: func(o *Options) { o.Beacon = nil }, contains: "beacon"},
		{
			name:     "default settings",
			mod:      func(o *Options) { o.DefaultSettings = fleet.Settings{} },
			contains: "default settings",
		},
		{
			name:     "plain http public URL",
			mod:      func(o *Options) { o.PublicURLs = []string{"http://upgradoor.example"} },
			contains: "plain http",
		},
		{
			name:     "public URL without scheme",
			mod:      func(o *Options) { o.PublicURLs = []string{"upgradoor.example"} },
			contains: "public URL",
		},
		{
			name:     "public URL with query",
			mod:      func(o *Options) { o.PublicURLs = []string{"https://upgradoor.example/?a=b"} },
			contains: "public URL",
		},
		{
			name:     "public URL with credentials",
			mod:      func(o *Options) { o.PublicURLs = []string{"https://u:p@upgradoor.example"} },
			contains: "public URL",
		},
		{
			name:     "public URL with another scheme",
			mod:      func(o *Options) { o.PublicURLs = []string{"ftp://upgradoor.example"} },
			contains: "http or https",
		},
		{
			name:     "auth provider that is not a URL",
			mod:      func(o *Options) { o.AuthProviderURL = "auth.example" },
			contains: "auth provider URL",
		},
		{
			name: "metrics registered twice",
			mod: func(o *Options) {
				reg := prometheus.NewRegistry()
				o.Registry = reg

				first := valid()
				first.Registry = reg

				_, ferr := New(log, first)
				require.NoError(t, ferr)
			},
			contains: "register metrics",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := valid()
			tt.mod(&opts)

			_, nerr := New(log, opts)
			require.ErrorContains(t, nerr, tt.contains)
		})
	}

	_, err = New(nil, valid())
	require.ErrorContains(t, err, "logger")

	// Plain http public URLs are accepted when explicitly allowed.
	opts := valid()
	opts.AllowInsecureAgentHTTP = true
	opts.PublicURLs = []string{"http://localhost:8080/"}

	_, err = New(log, opts)
	require.NoError(t, err)
}

func TestHealthz(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.get("/healthz")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, httpjson.ContentTypeJSON, rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())

	rec = h.sendJSON(http.MethodPost, "/healthz", "{}")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestAPIHeaders(t *testing.T) {
	h := newHarness(t, nil)

	for _, path := range []string{
		"/api/v1/config", "/api/v1/public/hosts", "/api/v1/hosts", "/api/v1/nope", "/healthz",
		"/api/v1/public/events",
	} {
		req := httptest.NewRequest(http.MethodGet, controllertest.BaseURL+path, http.NoBody)
		// A cross-origin caller gets no CORS grant.
		req.Header.Set("Origin", "https://evil.example")

		rec := h.serve(req)
		assert.Equal(t, httpjson.ContentTypeJSON, rec.Header().Get("Content-Type"), path)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), path)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), path)

		for name := range rec.Header() {
			assert.False(t, strings.HasPrefix(name, "Access-Control-"), "%s: %s", path, name)
		}
	}

	// A CORS preflight is not answered as one.
	req := httptest.NewRequest(http.MethodOptions, controllertest.BaseURL+"/api/v1/actions/roll", http.NoBody)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")

	rec := h.serve(req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestWebUI(t *testing.T) {
	const index = "<!doctype html><title>upgradoor ui</title>"

	ui := fstest.MapFS{
		"index.html":               {Data: []byte(index)},
		"assets/index-BX3k2a9d.js": {Data: []byte("console.log('ui')")},
	}

	h := newHarness(t, func(o *Options) {
		o.WebFS = ui
		o.AuthProviderURL = "https://auth.testnet.example"
	})

	tests := []struct {
		name   string
		path   string
		status int
		ctype  string
		body   string
	}{
		{name: "root", path: "/", status: 200, ctype: "text/html; charset=utf-8", body: index},
		{
			name: "client route", path: "/rollouts", status: 200,
			ctype: "text/html; charset=utf-8", body: index,
		},
		{
			name: "asset", path: "/assets/index-BX3k2a9d.js", status: 200,
			ctype: "text/javascript; charset=utf-8", body: "console.log('ui')",
		},
		{
			name: "missing asset", path: "/assets/gone.js", status: 404,
			ctype: "text/plain; charset=utf-8", body: "not found\n",
		},
		{
			name: "unknown api path is json, not the UI", path: "/api/v1/nope", status: 404,
			ctype: httpjson.ContentTypeJSON, body: `{"error":"not found"}` + "\n",
		},
		{
			// In the agent protocol's error shape, see TestAgentUnknownPath.
			name: "unknown agent api path", path: "/api/agent/v2/sync", status: 404,
			ctype: httpjson.ContentTypeJSON,
			body:  `{"error":"not found"}` + "\n",
		},
		{
			name: "api prefix alone", path: "/api/", status: 404,
			ctype: httpjson.ContentTypeJSON, body: `{"error":"not found"}` + "\n",
		},
		{
			name: "host detail without a name", path: "/api/v1/hosts/", status: 404,
			ctype: httpjson.ContentTypeJSON, body: `{"error":"not found"}` + "\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := h.get(tt.path)
			assert.Equal(t, tt.status, rec.Code)
			assert.Equal(t, tt.ctype, rec.Header().Get("Content-Type"))
			assert.Equal(t, tt.body, rec.Body.String())
		})
	}

	// UI pages carry the security headers, with the auth provider allowed.
	rec := h.get("/")
	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "script-src 'self' https://auth.testnet.example")
	assert.Contains(t, csp, "frame-ancestors 'none'")
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	// Nothing is injected into the page: the UI asks /api/v1/config.
	assert.NotContains(t, rec.Body.String(), controllertest.Network)

	// Without a UI: the placeholder.
	h = newHarness(t, nil)

	rec = h.get("/")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "does not include the web UI")
}

// metricsFixture is the leak fixture plus one open lease and rollouts.
func metricsFixture(t *testing.T, h *harness) {
	t.Helper()

	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	h.store.Hosts["lighthouse-geth-2"].LastSeen = controllertest.Epoch.Add(-10 * time.Minute)
	h.store.Leases = append(h.store.Leases,
		&fleet.Lease{ID: "l-open", Host: "lighthouse-geth-2", State: fleet.LeaseRunning})
	h.store.Rollouts = []*fleet.Rollout{
		{ID: "r1", State: fleet.RolloutActive},
		{ID: "r2", State: fleet.RolloutActive},
		{ID: "r3", State: fleet.RolloutCompleted},
	}
}

func TestMetrics(t *testing.T) {
	h := newHarness(t, nil)
	metricsFixture(t, h)

	agent := controllertest.ValidatorAgent(t)

	// One good sync (which also refreshes the host) and one rejected one.
	req := controllertest.SyncRequest()
	req.OS.RebootRequired = true
	req.Pending.Security = 4

	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{}).Code)
	require.Equal(t, http.StatusUnauthorized,
		h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{
			ProofURL: "https://elsewhere.example" + protocol.PathSync,
		}).Code)
	require.Equal(t, http.StatusBadRequest,
		h.agentPost(agent, protocol.PathInventory, map[string]string{"hash": "x"},
			controllertest.CallOpts{}).Code)

	rec := h.get("/metrics")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")

	body := rec.Body.String()

	for _, line := range []string{
		"upgradoor_hosts 3",
		"upgradoor_hosts_stale 1",
		"upgradoor_hosts_reboot_required 2",
		"upgradoor_leases_open 1",
		`upgradoor_rollouts{state="active"} 2`,
		`upgradoor_rollouts{state="halted"} 0`,
		"upgradoor_network_gate_ok 0",
		"upgradoor_network_participation_ratio 0.95",
		"upgradoor_network_finality_lag_epochs 2",
		"upgradoor_budget_unavailable_weight 10",
		"upgradoor_budget_allowed_weight 20",
		`upgradoor_agent_requests_total{code="200",endpoint="sync"} 1`,
		`upgradoor_agent_requests_total{code="401",endpoint="sync"} 1`,
		`upgradoor_agent_requests_total{code="400",endpoint="inventory"} 1`,
	} {
		assert.Contains(t, body, line+"\n")
	}

	// /metrics is unauthenticated: no host names, versions, kernels, addresses
	// or hashes.
	assert.NotContains(t, body, "host=")

	for _, secret := range controllertest.LeakSecrets {
		assert.NotContains(t, body, secret)
	}

	// A gate that is open reads 1.
	h.sched.Set(func(f *controllertest.Scheduler) {
		plan := *f.CurrentPlan
		plan.Gate.OK = true
		f.CurrentPlan = &plan
	})
	assert.Contains(t, h.get("/metrics").Body.String(), "upgradoor_network_gate_ok 1\n")
}

func TestMetricsBeforeFirstPlanAndOnStoreError(t *testing.T) {
	h := newHarness(t, nil)
	h.beacon.Network = fleet.NetworkHealth{FinalityLagEpochs: 4, Tracked: 10, Live: 5}

	body := h.get("/metrics").Body.String()
	assert.Contains(t, body, "upgradoor_hosts 0\n")
	assert.Contains(t, body, "upgradoor_network_gate_ok 0\n")
	assert.Contains(t, body, "upgradoor_network_participation_ratio 0.5\n")
	assert.Contains(t, body, "upgradoor_network_finality_lag_epochs 4\n")
	assert.NotContains(t, body, "upgradoor_budget_allowed_weight")

	// A failing store drops its groups; the scrape itself still works.
	h.store.SetErr(assert.AnError)

	rec := h.get("/metrics")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "upgradoor_hosts ")
	assert.NotContains(t, rec.Body.String(), "upgradoor_leases_open")
	assert.Contains(t, rec.Body.String(), "upgradoor_network_gate_ok 0\n")
}

// TestEveryUserRouteIsGuarded walks the routes the server registered behind
// the user authentication, not a list kept in the tests: without a token
// every one answers 401; with a token that has basic access only, every
// reading route answers and every other route answers 403, whatever its body.
func TestEveryUserRouteIsGuarded(t *testing.T) {
	h, provider := authHarness(t, true)
	controllertest.OwnershipFixture(h.store)

	routes := h.srv.userRoutes
	require.NotEmpty(t, routes)

	basic := map[string]string{
		"no services claim":       provider.Grant(t, ""),
		"basic":                   provider.Grant(t, "+upgradoor"),
		"basic, in a team":        provider.Grant(t, "-all,+upgradoor", "geth", "lighthouse"),
		"features of another one": provider.Grant(t, "+spamoor:all", "geth"),
	}
	staff := provider.Grant(t, controllertest.StaffServices)

	// adminReads are the reading routes that need a feature: admin, details
	// for the packages of the fleet, audit for the audit log.
	adminReads := []string{
		"GET /api/v1/settings", "GET /api/v1/settings/sources", "GET /api/v1/packages", "GET /api/v1/audit",
	}
	mutating := 0

	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			rec := h.serve(routeRequest(route, ""))
			require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
			assert.Equal(t, `Bearer realm="upgradoor"`, rec.Header().Get("WWW-Authenticate"))

			read := strings.HasPrefix(route, http.MethodGet+" ")
			if !read {
				mutating++
			}

			for name, authorization := range basic {
				got := h.serve(routeRequest(route, authorization))

				if read && !slices.Contains(adminReads, route) {
					// Some need a query; none refuses the login.
					assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden},
						got.Code, "%s: %s", name, got.Body.String())

					continue
				}

				require.Equal(t, http.StatusForbidden, got.Code, "%s: %s", name, got.Body.String())
				assert.True(t, strings.HasPrefix(controllertest.DecodeJSON[httpjson.ErrorBody](t, got).Error,
					"forbidden: this login lacks the feature upgradoor:"), got.Body.String())
			}

			// The same request by a login with every feature is not refused
			// for what the login may do.
			rec = h.serve(routeRequest(route, staff))
			assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code,
				rec.Body.String())
		})
	}

	// The actions, the settings, the registry and the validator ranges refresh.
	assert.Equal(t, len(actionNames(h))+3, mutating)

	for _, name := range actionNames(h) {
		assert.Contains(t, routes, "POST /api/v1/actions/"+name)
	}

	// Nothing a basic login sent reached the scheduler; what did was the
	// staff login's (most of its empty bodies fail validation).
	for _, call := range h.sched.Recorded() {
		assert.Equal(t, scheduler.Authorization{By: authz.FeatureAdmin}, call.Auth, call.Name)
	}

	// The refusals of the basic login are audited, nothing else is.
	for _, e := range h.store.AuditEntries() {
		assert.Equal(t, "auth.refused", e.Action, e.Target)
	}
}

// TestTeamFromAgentToViews: what an agent reports as a container's team
// reaches the authenticated host views, cleaned, and no public view.
func TestTeamFromAgentToViews(t *testing.T) {
	h, provider, _ := realHarness(t)

	const hidden = "qq-hidden-team"

	syncOwned(t, h, controllertest.HostMixed,
		controllertest.Owned("geth", protocol.KindEL, hidden),
		protocol.Container{
			Name: "lighthouse", Image: "sigp/lighthouse:unstable", Digest: "sha256:c1", State: "running",
			Managed: true, Kind: protocol.KindCL, Team: "lighthouse",
		},
		// Names no team can have.
		controllertest.Owned("bad", protocol.KindEL, "Not A Team"),
		controllertest.Owned("grant", protocol.KindEL, "geth:all"),
		// A team without a kind is nobody's.
		controllertest.Owned("unlabelled", "", "geth"),
		// Not managed: it has no team, whatever the agent says.
		protocol.Container{Name: "debug", State: "running", Kind: protocol.KindEL, Team: "lighthouse"},
		controllertest.Owned("nobody", "snooper-el", ""),
	)

	// An agent that reports no team.
	old := controllertest.NewAgent(t, identity.CertificateSpec{Hostname: controllertest.HostUnowned})
	body := `{"protocolVersion":1,"agentVersion":"agent-0.9.0","sentAt":"2026-11-14T12:00:00Z",` +
		`"bootId":"boot-1","verbs":["roll"],"containers":[{"name":"bootnode","id":"1",` +
		`"image":"ethpandaops/bootnode:latest","imageId":"sha256:1","state":"running",` +
		`"restartCount":0,"startedAt":"2026-11-14T11:00:00Z","managed":true,"kind":"bootnode"}]}`
	rec := h.serve(h.agentRequest(old, protocol.PathSync, []byte(body), controllertest.CallOpts{}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The full view: the unmanaged containers are part of it.
	reader := provider.Grant(t, "+upgradoor:details")

	owners := func(host views.HostListing) map[string]string {
		out := make(map[string]string, len(host.Containers))
		for _, c := range host.Containers {
			out[string(c.Name)] = c.Team
		}

		return out
	}

	want := map[string]string{
		"geth": hidden, "lighthouse": "lighthouse", "bad": "", "grant": "", "unlabelled": "",
		"debug": "", "nobody": "",
	}

	rec = h.serve(userRoute{method: http.MethodGet, path: "/api/v1/hosts"}.request(reader))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	list := controllertest.DecodeJSON[views.Hosts](t, rec)
	require.Len(t, list.Hosts, 2)
	assert.Equal(t, controllertest.HostUnowned, list.Hosts[0].Name)
	assert.Equal(t, map[string]string{"bootnode": ""}, owners(list.Hosts[0]))
	assert.Equal(t, want, owners(list.Hosts[1]))
	assert.NotContains(t, rec.Body.String(), `"team":""`, "no team is no field")

	rec = h.serve(userRoute{
		method: http.MethodGet, path: "/api/v1/hosts/" + controllertest.HostMixed,
	}.request(reader))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	summary := controllertest.DecodeJSON[views.HostDetail](t, rec).HostSummary
	assert.Equal(t, want, owners(views.Listing(&summary)))

	// The public views show the host and its images, and no team.
	for _, path := range []string{"/api/v1/public/hosts", "/api/v1/public/images"} {
		rec := h.get(path)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), "sigp/lighthouse:unstable")
		assert.NotContains(t, rec.Body.String(), hidden, path)
		assert.NotContains(t, rec.Body.String(), `"team"`, path)
	}

	// Without a token the authenticated view is closed.
	require.Equal(t, http.StatusUnauthorized, h.get("/api/v1/hosts").Code)
}

// The service name is checked before anything is built.
func TestAuthServiceName(t *testing.T) {
	h := newHarness(t, nil)

	for _, bad := range []string{"Upgradoor", "upgradoor:all", "up*", "+upgradoor", "a,b", " ", "a.b"} {
		opts := h.srv.opts
		opts.AuthService = bad
		opts.Registry = nil

		_, err := New(h.srv.log, opts)
		require.ErrorContains(t, err, "is not a service name", bad)
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

// A path or a method no handler serves is the web UI's: a JSON 404 under
// /api/.
func TestUnknownRoutes(t *testing.T) {
	h := newHarness(t, nil)

	// Unknown action.
	rec := h.sendJSON(http.MethodPost, "/api/v1/actions/explode", `{}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, httpjson.ContentTypeJSON, rec.Header().Get("Content-Type"))

	// Wrong method on a known action: no such route.
	rec = h.get("/api/v1/actions/roll")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// The profiles are mounted only when enabled, never without an auth
// provider, and a browser without a login gets the web UI as the gate.
func TestPprofRoutes(t *testing.T) {
	off, _ := authHarness(t, true)
	assert.NotEqual(t, http.StatusUnauthorized, off.get("/debug/pprof/heap").Code, "off: the web UI answers")

	c := controllertest.NewClock(controllertest.Epoch)
	provider := controllertest.NewAuthProvider(t, c, true)
	h := newHarness(t, func(o *Options) {
		o.AuthProviderURL = provider.Server.URL + "/"
		o.Now = c.Now
		o.Pprof = true
	})
	h.clock = c
	h.start()

	rec := h.get("/debug/pprof")
	assert.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.Equal(t, "/debug/pprof/", rec.Header().Get("Location"))

	assert.Equal(t, http.StatusUnauthorized, h.get("/debug/pprof/heap").Code)

	req := httptest.NewRequest(http.MethodGet, controllertest.BaseURL+"/debug/pprof/heap", http.NoBody)
	req.Header.Set("Accept", "text/html")
	rec = h.serve(req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, rec.Body.String(), `<head><base href="/">`, "the assets load from the root")

	req = httptest.NewRequest(http.MethodGet, controllertest.BaseURL+"/debug/pprof/heap", http.NoBody)
	req.Header.Set("Authorization", provider.Grant(t, "+upgradoor:admin"))
	rec = h.serve(req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))

	log := logrus.New()
	log.SetOutput(io.Discard)

	_, err := New(log, Options{
		Network: controllertest.Network, Verifier: h.srv.opts.Verifier, Store: h.store, Scheduler: h.sched,
		Beacon: h.beacon, DefaultSettings: fleet.DefaultSettings(), Pprof: true,
	})
	require.ErrorContains(t, err, "pprof needs an auth provider")
}
