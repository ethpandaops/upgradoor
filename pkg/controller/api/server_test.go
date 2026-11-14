package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestNewValidatesOptions(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	signer, err := identity.DeriveSignerKey([]byte(testSeed), testNetwork)
	require.NoError(t, err)

	valid := func() Options {
		return Options{
			Network:         testNetwork,
			Verifier:        identity.NewVerifier(identity.PublicKey(signer), testNetwork),
			Store:           newFakeStore(),
			Scheduler:       newFakeScheduler(),
			Beacon:          &fakeBeacon{},
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
	assert.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())

	rec = h.sendJSON(http.MethodPost, "/healthz", "{}")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestAPIHeaders(t *testing.T) {
	h := newHarness(t, nil)

	for _, path := range []string{
		"/api/v1/config", "/api/v1/public/hosts", "/api/v1/hosts", "/api/v1/nope", "/healthz",
	} {
		req := httptest.NewRequest(http.MethodGet, testBaseURL+path, http.NoBody)
		// A cross-origin caller gets no CORS grant.
		req.Header.Set("Origin", "https://evil.example")

		rec := h.serve(req)
		assert.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"), path)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), path)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), path)

		for name := range rec.Header() {
			assert.False(t, strings.HasPrefix(name, "Access-Control-"), "%s: %s", path, name)
		}
	}

	// A CORS preflight is not answered as one.
	req := httptest.NewRequest(http.MethodOptions, testBaseURL+"/api/v1/actions/roll", http.NoBody)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")

	rec := h.serve(req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestMethodChecks(t *testing.T) {
	h := newHarness(t, nil)

	tests := []struct {
		method string
		path   string
		allow  string
	}{
		{method: http.MethodPost, path: "/api/v1/config", allow: "GET"},
		{method: http.MethodDelete, path: "/api/v1/public/hosts", allow: "GET"},
		{method: http.MethodPost, path: "/api/v1/hosts", allow: "GET"},
		{method: http.MethodDelete, path: "/api/v1/hosts/lighthouse-geth-1", allow: "GET"},
		{method: http.MethodPost, path: "/api/v1/settings", allow: "GET, PUT"},
		{method: http.MethodPut, path: "/api/v1/actions/forget", allow: "POST"},
		{method: http.MethodPost, path: "/metrics", allow: "GET"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := h.sendJSON(tt.method, tt.path, "{}")
			require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			assert.Equal(t, tt.allow, rec.Header().Get("Allow"))
			assert.Empty(t, h.sched.recorded())
		})
	}
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
			ctype: contentTypeJSON, body: `{"error":"not found"}` + "\n",
		},
		{
			// In the agent protocol's error shape, see TestAgentUnknownPath.
			name: "unknown agent api path", path: "/api/agent/v2/sync", status: 404,
			ctype: contentTypeJSON,
			body:  `{"error":"not found","serverTime":"2026-11-14T12:00:00Z"}` + "\n",
		},
		{
			name: "api prefix alone", path: "/api/", status: 404,
			ctype: contentTypeJSON, body: `{"error":"not found"}` + "\n",
		},
		{
			name: "host detail without a name", path: "/api/v1/hosts/", status: 404,
			ctype: contentTypeJSON, body: `{"error":"not found"}` + "\n",
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
	assert.NotContains(t, rec.Body.String(), testNetwork)

	// Without a UI: the placeholder.
	h = newHarness(t, nil)

	rec = h.get("/")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "does not include the web UI")
}

// metricsFixture is the leak fixture plus one open lease and rollouts.
func metricsFixture(t *testing.T, h *harness) {
	t.Helper()

	leakFixture(t, h)

	h.store.hosts["lighthouse-geth-2"].LastSeen = testEpoch.Add(-10 * time.Minute)
	h.store.leases = append(h.store.leases,
		&fleet.Lease{ID: "l-open", Host: "lighthouse-geth-2", State: fleet.LeaseRunning})
	h.store.rollouts = []*fleet.Rollout{
		{ID: "r1", State: fleet.RolloutActive},
		{ID: "r2", State: fleet.RolloutActive},
		{ID: "r3", State: fleet.RolloutCompleted},
	}
}

func TestMetrics(t *testing.T) {
	h := newHarness(t, nil)
	metricsFixture(t, h)

	// The fixture knows the host at generation 3; an older certificate would
	// be rejected.
	agent := validatorAgentGeneration(t, 3)

	// One good sync (which also refreshes the host) and one rejected one.
	req := syncRequest()
	req.OS.RebootRequired = true
	req.Pending.Security = 4

	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, req, agentCallOpts{}).Code)
	require.Equal(t, http.StatusUnauthorized,
		h.agentPost(agent, protocol.PathSync, req, agentCallOpts{
			ProofURL: "https://elsewhere.example" + protocol.PathSync,
		}).Code)
	require.Equal(t, http.StatusBadRequest,
		h.agentPost(agent, protocol.PathInventory, map[string]string{"hash": "x"},
			agentCallOpts{}).Code)

	rec := h.get("/metrics")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")

	body := rec.Body.String()

	for _, line := range []string{
		"upgradoor_hosts 3",
		"upgradoor_hosts_stale 1",
		"upgradoor_hosts_reboot_required 2",
		`upgradoor_host_pending_security_updates{host="lighthouse-geth-1"} 4`,
		`upgradoor_host_pending_security_updates{host="lighthouse-geth-2"} 4`,
		`upgradoor_host_last_seen_timestamp_seconds{host="lighthouse-geth-1"} ` +
			strconv.FormatFloat(float64(testEpoch.Unix()), 'g', -1, 64),
		`upgradoor_host_validators_live{host="lighthouse-geth-1"} 90`,
		`upgradoor_host_validators_total{host="lighthouse-geth-1"} 100`,
		`upgradoor_host_validators_total{host="bootnode-1"} 0`,
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

	// Liveness of a host the plan knows nothing about is absent, not zero.
	assert.NotContains(t, body, `upgradoor_host_validators_live{host="lighthouse-geth-2"}`)
	assert.NotContains(t, body, `upgradoor_host_pending_security_updates{host="bootnode-1"}`)

	// /metrics is unauthenticated: no versions, kernels, addresses or hashes.
	for _, secret := range leakSecrets {
		assert.NotContains(t, body, secret)
	}

	// A gate that is open reads 1.
	h.sched.set(func(f *fakeScheduler) {
		plan := *f.plan
		plan.Gate.OK = true
		f.plan = &plan
	})
	assert.Contains(t, h.get("/metrics").Body.String(), "upgradoor_network_gate_ok 1\n")
}

func TestMetricsBeforeFirstPlanAndOnStoreError(t *testing.T) {
	h := newHarness(t, nil)
	h.beacon.health = fleet.NetworkHealth{FinalityLagEpochs: 4, Tracked: 10, Live: 5}

	body := h.get("/metrics").Body.String()
	assert.Contains(t, body, "upgradoor_hosts 0\n")
	assert.Contains(t, body, "upgradoor_network_gate_ok 0\n")
	assert.Contains(t, body, "upgradoor_network_participation_ratio 0.5\n")
	assert.Contains(t, body, "upgradoor_network_finality_lag_epochs 4\n")
	assert.NotContains(t, body, "upgradoor_budget_allowed_weight")

	// A failing store drops its groups; the scrape itself still works.
	h.store.setErr(assert.AnError)

	rec := h.get("/metrics")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "upgradoor_hosts ")
	assert.NotContains(t, rec.Body.String(), "upgradoor_leases_open")
	assert.Contains(t, rec.Body.String(), "upgradoor_network_gate_ok 0\n")
}

func TestMetricNames(t *testing.T) {
	h := newHarness(t, nil)
	metricsFixture(t, h)

	require.Equal(t, http.StatusOK,
		h.agentPost(validatorAgentGeneration(t, 3), protocol.PathSync, syncRequest(),
			agentCallOpts{}).Code)

	problems, err := testutil.GatherAndLint(h.reg)
	require.NoError(t, err)

	// upgradoor_host_validators_total is a gauge under the name the metric
	// list of this project specifies; promlint reserves the suffix for
	// counters. Every other metric must be clean.
	for _, p := range problems {
		assert.Equal(t, "upgradoor_host_validators_total", p.Metric, p.Text)
	}

	count, err := testutil.GatherAndCount(h.reg)
	require.NoError(t, err)
	assert.Positive(t, count)
}
