package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// harness is a Server over fakes.
type harness struct {
	t      *testing.T
	srv    *Server
	store  *controllertest.Store
	sched  *controllertest.Scheduler
	beacon *controllertest.Beacon
	clock  *controllertest.Clock
	reg    *prometheus.Registry
}

// newHarness builds a Server in open mode; mod can change the options.
func newHarness(t *testing.T, mod func(*Options)) *harness {
	t.Helper()

	h := &harness{
		t:      t,
		store:  controllertest.NewStore(),
		sched:  controllertest.NewScheduler(),
		beacon: &controllertest.Beacon{},
		clock:  controllertest.NewClock(controllertest.Epoch),
		reg:    prometheus.NewRegistry(),
	}

	signer, err := identity.DeriveSignerKey([]byte(controllertest.Seed), controllertest.Network)
	require.NoError(t, err)

	opts := Options{
		Network: controllertest.Network,
		Version: "v1.2.3-test",
		Verifier: identity.NewVerifier(identity.PublicKey(signer), controllertest.Network,
			identity.WithClock(h.clock.Now)),
		Store:           h.store,
		Scheduler:       h.sched,
		Beacon:          h.beacon,
		DefaultSettings: fleet.DefaultSettings(),
		Now:             h.clock.Now,
		Registry:        h.reg,
	}

	if mod != nil {
		mod(&opts)
	}

	log := logrus.New()
	log.SetOutput(io.Discard)

	h.srv, err = New(log, opts)
	require.NoError(t, err)

	return h
}

// start starts the server and stops it at the end of the test.
func (h *harness) start() {
	h.t.Helper()

	require.NoError(h.t, h.srv.Start(context.Background()))
	h.t.Cleanup(func() { require.NoError(h.t, h.srv.Stop()) })
}

// serve runs one request through the handler.
func (h *harness) serve(req *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()

	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)

	return rec
}

// get runs a GET without credentials.
func (h *harness) get(target string) *httptest.ResponseRecorder {
	h.t.Helper()

	return h.serve(httptest.NewRequest(http.MethodGet, controllertest.BaseURL+target, http.NoBody))
}

// sendJSON runs a request with a JSON body and no credentials.
func (h *harness) sendJSON(method, target, body string) *httptest.ResponseRecorder {
	h.t.Helper()

	req := httptest.NewRequest(method, controllertest.BaseURL+target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	return h.serve(req)
}

// agentRequest builds a signed agent request on the harness's clock.
func (h *harness) agentRequest(
	a *controllertest.Agent, path string, body []byte, o controllertest.CallOpts,
) *http.Request {
	h.t.Helper()

	return a.Request(h.t, h.clock.Now(), path, body, o)
}

// agentPost signs and serves an agent request with v as JSON body.
func (h *harness) agentPost(
	a *controllertest.Agent, path string, v any, o controllertest.CallOpts,
) *httptest.ResponseRecorder {
	h.t.Helper()

	body, err := json.Marshal(v)
	require.NoError(h.t, err)

	return h.serve(h.agentRequest(a, path, body, o))
}

// authHarness is a started Server with an auth provider.
func authHarness(t *testing.T, discovery bool) (*harness, *controllertest.AuthProvider) {
	t.Helper()

	c := controllertest.NewClock(controllertest.Epoch)
	provider := controllertest.NewAuthProvider(t, c, discovery)

	h := newHarness(t, func(o *Options) {
		o.AuthProviderURL = provider.Server.URL + "/"
		o.Now = c.Now
	})
	h.clock = c
	h.start()

	return h, provider
}

// realHarness is a Server over the real store and the real scheduler, with an
// auth provider.
func realHarness(t *testing.T) (*harness, *controllertest.AuthProvider, store.Store) {
	t.Helper()

	log := logrus.New()
	log.SetOutput(io.Discard)

	st := store.NewMemory(log)
	require.NoError(t, st.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, st.Stop()) })

	c := controllertest.NewClock(controllertest.Epoch)
	provider := controllertest.NewAuthProvider(t, c, true)

	h := newHarness(t, func(o *Options) {
		o.AuthProviderURL = provider.Server.URL
		o.Now = c.Now
		o.Store = st
		o.Scheduler = scheduler.New(log, scheduler.Options{
			Store:    st,
			Health:   func() fleet.NetworkHealth { return fleet.NetworkHealth{} },
			Defaults: fleet.DefaultSettings(),
			Now:      c.Now,
		})
	})
	h.clock = c
	h.start()

	return h, provider, st
}

// syncOwned sends a host's sync through the agent API.
func syncOwned(t *testing.T, h *harness, host string, containers ...protocol.Container) {
	t.Helper()

	req := controllertest.SyncRequest()
	req.Containers = containers

	rec := h.agentPost(controllertest.NewAgent(t, identity.CertificateSpec{Hostname: host}), protocol.PathSync,
		req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// userRoute is one request to a user route.
type userRoute struct {
	method string
	path   string
	body   string
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

	// An event stream is answered until the client goes away: this one does
	// soon, and what was recorded until then is the answer.
	if strings.HasPrefix(r.path, "/api/v1/events") {
		ctx, cancel := context.WithTimeout(req.Context(), 50*time.Millisecond)
		context.AfterFunc(ctx, cancel)

		req = req.WithContext(ctx)
	}

	return req
}

// routeRequest builds a request for one registered user route, as
// "METHOD pattern", with a body that is valid JSON and names nothing.
func routeRequest(route, authorization string) *http.Request {
	method, pattern, _ := strings.Cut(route, " ")

	r := userRoute{method: method, path: strings.ReplaceAll(pattern, "{name}", controllertest.HostMixed)}
	if method != http.MethodGet {
		r.body = "{}"
	}

	return r.request(authorization)
}

// actionNames are the actions the user API registers.
func actionNames(h *harness) []string {
	var out []string

	for _, route := range h.srv.users.Routes() {
		if name, ok := strings.CutPrefix(route.Pattern, "/api/v1/actions/"); ok {
			out = append(out, name)
		}
	}

	return out
}
