package stream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/userapi"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

// harnessOptions are what a test may change of the harness.
type harnessOptions struct {
	// AuthProviderURL is the authenticatoor of the user API; empty is open.
	AuthProviderURL string
	// Now is the clock.
	Now func() time.Time
}

// harness is a Handler over fakes, mounted the way the server mounts it:
// the authenticated stream behind the user authentication, next to the REST
// views of the user API it pushes.
type harness struct {
	t       *testing.T
	srv     *Handler
	users   *userapi.Handler
	views   *views.Views
	handler http.Handler
	store   *controllertest.Store
	sched   *controllertest.Scheduler
	beacon  *controllertest.Beacon
	clock   *controllertest.Clock
}

// newHarness builds the streams; mod can change the options.
func newHarness(t *testing.T, mod func(*harnessOptions)) *harness {
	t.Helper()

	h := &harness{
		t:      t,
		store:  controllertest.NewStore(),
		sched:  controllertest.NewScheduler(),
		beacon: &controllertest.Beacon{},
		clock:  controllertest.NewClock(controllertest.Epoch),
	}

	ho := harnessOptions{Now: h.clock.Now}
	if mod != nil {
		mod(&ho)
	}

	log := logrus.New()
	log.SetOutput(io.Discard)

	h.views = views.New(views.Options{Store: h.store, Scheduler: h.sched, Beacon: h.beacon, Now: ho.Now})
	h.srv = New(log, Options{
		Store: h.store, Scheduler: h.sched, Beacon: h.beacon, Views: h.views,
		DefaultSettings: fleet.DefaultSettings(), Now: ho.Now,
	})
	h.users = userapi.New(log, userapi.Options{
		Network: controllertest.Network, AuthProviderURL: strings.TrimRight(ho.AuthProviderURL, "/"),
		AuthService: "upgradoor", Store: h.store, Scheduler: h.sched, Beacon: h.beacon, Views: h.views,
		DefaultSettings: fleet.DefaultSettings(), Notify: h.srv.Notify, Now: ho.Now,
	})

	mux := http.NewServeMux()

	for _, route := range h.users.Routes() {
		handler := route.Public
		if route.User != nil {
			handler = h.users.Guard(route.User)
		}

		mux.Handle(route.Method+" "+route.Pattern, handler)
	}

	mux.Handle("GET "+PathPublicEvents, http.HandlerFunc(h.srv.HandlePublic))
	mux.Handle("GET "+PathEvents, h.users.Guard(h.srv.Handle))
	h.handler = mux

	return h
}

// start starts the user authentication and the hub until the end of the
// test.
func (h *harness) start() {
	h.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)

	require.NoError(h.t, h.users.Start(context.Background(), ctx))
	require.NoError(h.t, h.srv.Start(ctx))
	h.t.Cleanup(func() { require.NoError(h.t, h.srv.Stop()) })
}

// serve runs one request through the routes.
func (h *harness) serve(req *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	return rec
}

// get runs a GET without credentials.
func (h *harness) get(target string) *httptest.ResponseRecorder {
	h.t.Helper()

	return h.serve(httptest.NewRequest(http.MethodGet, controllertest.BaseURL+target, http.NoBody))
}

// authHarness is a started harness with an auth provider.
func authHarness(t *testing.T, discovery bool) (*harness, *controllertest.AuthProvider) {
	t.Helper()

	c := controllertest.NewClock(controllertest.Epoch)
	provider := controllertest.NewAuthProvider(t, c, discovery)

	h := newHarness(t, func(o *harnessOptions) {
		o.AuthProviderURL = provider.Server.URL + "/"
		o.Now = c.Now
	})
	h.clock = c
	h.start()

	return h, provider
}
