package agentapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/metrics"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// harness is a Handler over fakes, its routes mounted the way the server
// mounts them, and /metrics next to them.
type harness struct {
	t       *testing.T
	srv     *Handler
	views   *views.Views
	handler http.Handler
	store   *controllertest.Store
	sched   *controllertest.Scheduler
	beacon  *controllertest.Beacon
	lookups *controllertest.Lookups
	clock   *controllertest.Clock
	reg     *prometheus.Registry
	// notified counts the calls of Options.Notify.
	notified atomic.Int32
}

// newHarness builds a Handler; mod can change the options. PublicURLs are
// normalized as the server does.
func newHarness(t *testing.T, mod func(*Options)) *harness {
	t.Helper()

	h := &harness{
		t:       t,
		store:   controllertest.NewStore(),
		sched:   controllertest.NewScheduler(),
		beacon:  &controllertest.Beacon{},
		lookups: &controllertest.Lookups{},
		clock:   controllertest.NewClock(controllertest.Epoch),
		reg:     prometheus.NewRegistry(),
	}

	signer, err := identity.DeriveSignerKey([]byte(controllertest.Seed), controllertest.Network)
	require.NoError(t, err)

	log := logrus.New()
	log.SetOutput(io.Discard)

	h.views = views.New(views.Options{
		Store: h.store, Scheduler: h.sched, Beacon: h.beacon, Now: h.clock.Now,
	})

	collector, err := metrics.New(log, h.reg, metrics.Options{
		Store: h.store, Scheduler: h.sched, Gate: func() scheduler.Gate { return h.views.Gate() },
		Now: h.clock.Now,
	})
	require.NoError(t, err)

	opts := Options{
		Verifier: identity.NewVerifier(identity.PublicKey(signer), controllertest.Network,
			identity.WithClock(h.clock.Now)),
		Store:     h.store,
		Scheduler: h.sched,
		Lookups:   h.lookups,
		Metrics:   collector,
		Notify:    func() { h.notified.Add(1) },
		Now:       h.clock.Now,
	}

	if mod != nil {
		mod(&opts)
	}

	opts.PublicURLs, err = NormalizePublicURLs(opts.PublicURLs, opts.AllowInsecureAgentHTTP)
	require.NoError(t, err)

	h.srv = New(log, opts)

	mux := http.NewServeMux()

	for _, route := range h.srv.Routes() {
		mux.Handle(route.Method+" "+route.Pattern, route.Handler)
	}

	mux.Handle("GET /metrics", promhttp.HandlerFor(h.reg, promhttp.HandlerOpts{}))
	h.handler = mux

	return h
}

// serve runs one request through the routes.
func (h *harness) serve(req *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	return rec
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

// detail is the host's detail view, as the user API serves it.
func (h *harness) detail(name string) *views.HostDetail {
	h.t.Helper()

	detail, err := h.views.HostDetail(context.Background(), name)
	require.NoError(h.t, err)

	return detail
}
