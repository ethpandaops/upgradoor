package userapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethpandaops/service-authenticatoor/pkg/auth"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// actionSettings stands for PUT /api/v1/settings in the tables.
const actionSettings = "settings"

// harness is a Handler over fakes, its routes mounted the way the server
// mounts them.
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
	// notified counts the calls of Options.Notify.
	notified atomic.Int32
}

// newHarness builds a Handler in open mode; mod can change the options.
func newHarness(t *testing.T, mod func(*Options)) *harness {
	t.Helper()

	h := &harness{
		t:       t,
		store:   controllertest.NewStore(),
		sched:   controllertest.NewScheduler(),
		beacon:  &controllertest.Beacon{},
		lookups: &controllertest.Lookups{Refreshable: discovery.Refreshed{Tags: 3}},
		clock:   controllertest.NewClock(controllertest.Epoch),
	}

	opts := Options{
		Network:         controllertest.Network,
		Version:         "v1.2.3-test",
		AuthService:     "upgradoor",
		Store:           h.store,
		Scheduler:       h.sched,
		Lookups:         h.lookups,
		Beacon:          h.beacon,
		DefaultSettings: fleet.DefaultSettings(),
		Notify:          func() { h.notified.Add(1) },
		Now:             h.clock.Now,
	}

	if mod != nil {
		mod(&opts)
	}

	// What the server does with the options before it hands them on.
	opts.AuthProviderURL = strings.TrimRight(opts.AuthProviderURL, "/")
	opts.Views = views.New(views.Options{
		Store: opts.Store, Scheduler: opts.Scheduler, Beacon: opts.Beacon, Now: opts.Now,
	})
	h.views = opts.Views

	log := logrus.New()
	log.SetOutput(io.Discard)

	h.srv = New(log, opts)

	mux := http.NewServeMux()

	for _, route := range h.srv.Routes() {
		handler := route.Public
		if route.User != nil {
			handler = h.srv.Guard(route.User)
		}

		mux.Handle(route.Method+" "+route.Pattern, handler)
	}

	h.handler = mux

	return h
}

// start starts the handler (the token verifier) until the end of the test.
func (h *harness) start() {
	h.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)

	require.NoError(h.t, h.srv.Start(context.Background(), ctx))
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

// sendJSON runs a request with a JSON body and no credentials.
func (h *harness) sendJSON(method, target, body string) *httptest.ResponseRecorder {
	h.t.Helper()

	req := httptest.NewRequest(method, controllertest.BaseURL+target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	return h.serve(req)
}

// authHarness is a started Handler with an auth provider.
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

// realHarness is a Handler over the real store and the real scheduler, with
// an auth provider: what an action does is what it does in the controller.
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

// syncOwned stores a host's sync the way the agent API stores one.
func syncOwned(t *testing.T, st store.Store, host string, containers ...protocol.Container) {
	t.Helper()

	req := controllertest.SyncRequest()
	req.Containers = containers

	_, err := st.RecordSync(context.Background(), &store.HostSync{
		Cert:    &identity.Certificate{Subject: host, Network: controllertest.Network, Hash: "cert-" + host},
		Request: req,
		At:      controllertest.Epoch,
	})
	require.NoError(t, err)
}

// obeyHolds is the agent of host: it takes the hold and release-hold
// directives that wait for it into the holds it reports, and the controller
// takes its answers.
func obeyHolds(t *testing.T, st store.Store, host string) {
	t.Helper()

	ctx := context.Background()

	pending, err := st.ListDirectives(ctx, store.DirectiveFilter{Host: host, PendingOnly: true})
	require.NoError(t, err)

	h, err := st.GetHost(ctx, host)
	require.NoError(t, err)

	req := *h.Status
	holds := slices.Clone(req.Hold.Holds)
	answered := make([]*fleet.Directive, 0, len(pending))

	for _, d := range pending {
		if d.Kind != protocol.DirectiveHold && d.Kind != protocol.DirectiveReleaseHold {
			continue
		}

		holds = slices.DeleteFunc(holds, func(x protocol.HostHold) bool { return x.Holder == d.Holder })
		if d.Kind == protocol.DirectiveHold {
			holds = append(holds, protocol.HostHold{Holder: d.Holder, By: d.By.User, Reason: d.Reason, At: d.CreatedAt})
		}

		d.State = fleet.DirectiveAccepted
		answered = append(answered, d)
	}

	slices.SortFunc(holds, protocol.HoldOrder)
	req.Hold.Holds = holds

	_, err = st.RecordSync(ctx, &store.HostSync{
		Cert:    &identity.Certificate{Subject: host, Network: controllertest.Network, Hash: h.CertHash},
		Request: &req, At: controllertest.Epoch,
	})
	require.NoError(t, err)
	require.NoError(t, st.Commit(ctx, &store.Changes{Directives: answered}))
}

// teamsVerifier accepts every token and gives it the teams.
type teamsVerifier struct {
	teams []string
}

func (v teamsVerifier) Verify(string, ...auth.VerifyOption) (*auth.Claims, error) {
	return &auth.Claims{Email: controllertest.UserEmail, Services: controllertest.StaffServices, Teams: v.teams}, nil
}

// authzCase is one request by one user.
type authzCase struct {
	name   string
	action string
	body   string
	// services is the token's "services" claim; teams its teams; open stands
	// for a controller without an auth provider.
	services string
	teams    []string
	open     bool
	// by is what the audit log records for an allowed request. Empty means
	// the request is refused, with exactly refusal or, when that is empty,
	// with something that starts with refusalPrefix.
	by            string
	refusal       string
	refusalPrefix string
}

// settingsBody is a body of PUT /api/v1/settings that changes a setting of
// the defaults.
func settingsBody(t *testing.T) string {
	t.Helper()

	return `{"minPeers":7}`
}

// runAuthz sends the request of one case to the action's handler as the
// case's user: the handler the route serves, without the token verification
// in front of it.
func runAuthz(t *testing.T, h *harness, tt *authzCase) {
	t.Helper()

	handler, method := h.srv.handlePutSettings, http.MethodPut

	// Every settings case starts from the defaults, so its body is a change.
	h.sched.Set(func(f *controllertest.Scheduler) { f.CurrentSettings = fleet.DefaultSettings() })

	if tt.action != actionSettings {
		var ok bool

		handler, ok = h.srv.actions()[tt.action]
		require.True(t, ok, "no action %q", tt.action)

		method = http.MethodPost
	}

	req := httptest.NewRequest(method, controllertest.BaseURL+"/api/v1/x", strings.NewReader(tt.body))
	req.Header.Set("Content-Type", "application/json")

	u := &authz.User{Actor: controllertest.UserEmail, Open: tt.open, Teams: tt.teams, Services: tt.services}
	if tt.open {
		u.Actor = actorAnonymous
	}

	before := h.sched.Recorded()
	audit := h.store.AuditEntries()

	rec := httptest.NewRecorder()
	handler(rec, req, u)

	calls := h.sched.Recorded()

	if tt.by != "" {
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Len(t, calls, len(before)+1, "the scheduler was called once")

		call := calls[len(before)]
		assert.Equal(t, u.Actor, call.Actor)
		assert.Equal(t, scheduler.Authorization{By: tt.by, Teams: tt.teams}, call.Auth)

		return
	}

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	got := controllertest.DecodeJSON[httpjson.ErrorBody](t, rec).Error

	if tt.refusal != "" {
		assert.Equal(t, tt.refusal, got)
	} else {
		require.NotEmpty(t, tt.refusalPrefix, "the case names no expectation")
		assert.True(t, strings.HasPrefix(got, tt.refusalPrefix), "%q does not start with %q",
			got, tt.refusalPrefix)
	}

	assert.Len(t, calls, len(before), "a refused request does not reach the scheduler")

	notRefusals := func(entries []fleet.AuditEntry) []fleet.AuditEntry {
		return slices.DeleteFunc(entries, func(e fleet.AuditEntry) bool { return e.Action == "auth.refused" })
	}
	assert.Len(t, notRefusals(h.store.AuditEntries()), len(notRefusals(audit)),
		"a refused request is audited as a refusal only")
}

// actionSample is one request per kind of action, on targets the team
// lighthouse covers, with the feature the action declares.
type actionSample struct {
	action  string
	body    string
	feature string
	// twin: the feature has a team twin.
	twin bool
}

func actionSamples(t *testing.T) []actionSample {
	t.Helper()

	hosts := fmt.Sprintf(`{"hosts":[%q]}`, controllertest.HostMixed)
	rolloutHosts := fmt.Sprintf(`{"rollout":"r-1","hosts":[%q]}`, controllertest.HostMixed)

	return []actionSample{
		{
			action:  "roll",
			body:    fmt.Sprintf(`{"hosts":[%q],"containers":["lighthouse"]}`, controllertest.HostMixed),
			feature: authz.FeatureManage, twin: true,
		},
		{
			action:  "roll",
			body:    fmt.Sprintf(`{"hosts":[%q],"containers":["lighthouse"],"force":true,"ignoreBudget":true}`, controllertest.HostMixed),
			feature: authz.FeatureManage, twin: true,
		},
		{action: "reboot", body: hosts, feature: authz.FeatureManage},
		{action: "hold", body: hosts, feature: authz.FeatureManage, twin: true},
		{action: "unhold", body: hosts, feature: authz.FeatureManage, twin: true},
		{
			action:  "mark-offline",
			body:    fmt.Sprintf(`{"hosts":[%q],"reason":"debugging a client"}`, controllertest.HostMixed),
			feature: authz.FeatureManage,
		},
		{action: "unmark-offline", body: hosts, feature: authz.FeatureManage},
		{action: "skip", body: rolloutHosts, feature: authz.FeatureManage, twin: true},
		{action: "retry", body: rolloutHosts, feature: authz.FeatureManage, twin: true},
		{
			action: "end-lease", body: fmt.Sprintf(`{"lease":%q}`, controllertest.LeaseMixed),
			feature: authz.FeatureManage, twin: true,
		},
		{
			action:  "image-override",
			body:    fmt.Sprintf(`{"host":%q,"container":"lighthouse","image":"sigp/lighthouse:fix"}`, controllertest.HostMixed),
			feature: authz.FeatureManageImage, twin: true,
		},
		{
			action:  "flag-override",
			body:    fmt.Sprintf(`{"host":%q,"container":"lighthouse","addFlags":["--debug-level=debug"]}`, controllertest.HostMixed),
			feature: authz.FeatureManageFlags, twin: true,
		},
		{
			action: "client-override",
			body: fmt.Sprintf(`{"client":"lighthouse","kind":"cl","hosts":[%q],"part":"image","image":"sigp/lighthouse:fix"}`,
				controllertest.HostMixed),
			feature: authz.FeatureManageImage, twin: true,
		},
		{
			action: "client-override",
			body: fmt.Sprintf(`{"client":"lighthouse","kind":"vc","hosts":[%q],"part":"flags","addFlags":["--debug"]}`,
				controllertest.HostMixed),
			feature: authz.FeatureManageFlags, twin: true,
		},
		{
			action: "client-policy", body: `{"client":"lighthouse","policy":"manual"}`,
			feature: authz.FeatureManage, twin: true,
		},
		{action: "pause", body: `{"rollout":"r-lighthouse"}`, feature: authz.FeatureManage, twin: true},
		{action: "resume", body: `{"rollout":"r-lighthouse"}`, feature: authz.FeatureManage, twin: true},
		{action: "ignore-gate", body: `{"rollout":"r-lighthouse"}`, feature: authz.FeatureManage, twin: true},
		{action: "ignore-budget", body: `{"rollout":"r-lighthouse"}`, feature: authz.FeatureManage, twin: true},
		{
			action: "approve-lease", body: fmt.Sprintf(`{"rollout":"r-lighthouse","host":%q}`, controllertest.HostMixed),
			feature: authz.FeatureManage, twin: true,
		},
		{action: "abort", body: `{"rollout":"r-lighthouse"}`, feature: authz.FeatureManage, twin: true},
		{action: "retry", body: `{"rollout":"r-lighthouse"}`, feature: authz.FeatureManage, twin: true},
		{action: "pause", body: `{}`, feature: authz.FeatureAdmin},
		{action: "resume", body: `{"reason":"over"}`, feature: authz.FeatureAdmin},
		{action: "arm-deploy-window", body: `{}`, feature: authz.FeatureAdmin},
		{action: "forget", body: hosts, feature: authz.FeatureAdmin},
		{action: actionSettings, body: settingsBody(t), feature: authz.FeatureAdmin},
	}
}

// webPermissions is the page's copy of the rule, relative to this package.
const webPermissions = "../../../web/src/lib/permissions.ts"

// tsBlock returns the text between a line that starts a declaration and the
// line that closes it.
func tsBlock(t *testing.T, src, start, end string) string {
	t.Helper()

	_, rest, ok := strings.Cut(src, start)
	require.True(t, ok, "%s has no %q", webPermissions, start)

	block, _, ok := strings.Cut(rest, end)
	require.True(t, ok, "%q is not closed by %q", start, end)

	return block
}
