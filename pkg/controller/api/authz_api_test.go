package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// routeRequest builds a request for one registered user route, as
// "METHOD pattern", with a body that is valid JSON and names nothing.
func routeRequest(route, authorization string) *http.Request {
	method, pattern, _ := strings.Cut(route, " ")

	r := userRoute{method: method, path: strings.ReplaceAll(pattern, "{name}", hostMixed)}
	if method != http.MethodGet {
		r.body = "{}"
	}

	return r.request(authorization)
}

// TestEveryUserRouteIsGuarded walks the routes the server registered behind
// the user authentication, not a list kept in the tests: without a token
// every one answers 401; with a token that has basic access only, every
// reading route answers and every other route answers 403, whatever its body.
func TestEveryUserRouteIsGuarded(t *testing.T) {
	h, provider := authHarness(t, true)
	ownershipFixture(h)

	routes := h.srv.userRoutes
	require.NotEmpty(t, routes)

	basic := map[string]string{
		"no services claim":       provider.grant(t, ""),
		"basic":                   provider.grant(t, "+upgradoor"),
		"basic, in a team":        provider.grant(t, "-all,+upgradoor", "geth", "lighthouse"),
		"features of another one": provider.grant(t, "+spamoor:all", "geth"),
	}
	staff := provider.grant(t, staffServices)

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

				if read {
					// Some need a query; none refuses the login.
					assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden},
						got.Code, "%s: %s", name, got.Body.String())

					continue
				}

				require.Equal(t, http.StatusForbidden, got.Code, "%s: %s", name, got.Body.String())
				assert.True(t, strings.HasPrefix(decodeJSON[ErrorBody](t, got).Error,
					"forbidden: this login lacks the feature upgradoor:"), got.Body.String())
			}

			// The same request by a login with every feature is not refused
			// for what the login may do.
			rec = h.serve(routeRequest(route, staff))
			assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code,
				rec.Body.String())
		})
	}

	// The eleven actions and the settings.
	assert.Equal(t, len(h.srv.actions())+1, mutating)

	for name := range h.srv.actions() {
		assert.Contains(t, routes, "POST /api/v1/actions/"+name)
	}

	// Nothing a basic login sent reached the scheduler; what did was the
	// staff login's (most of its empty bodies fail validation).
	for _, call := range h.sched.recorded() {
		assert.Equal(t, scheduler.Authorization{By: FeatureAdmin}, call.Auth, call.Name)
	}

	assert.Empty(t, h.store.auditEntries())
}

// TestTokenClaimsAuthorize: the features and teams the handlers decide with
// are the ones of the verified token.
func TestTokenClaimsAuthorize(t *testing.T) {
	h, provider := authHarness(t, true)
	ownershipFixture(h)

	post := func(action, body, authorization string) *httptest.ResponseRecorder {
		return h.serve(userRoute{
			method: http.MethodPost, path: "/api/v1/actions/" + action, body: body,
		}.request(authorization))
	}

	holdMixed := `{"hosts":["` + hostMixed + `"],"reason":"debugging"}`

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
			authorization: provider.grant(t, "-all,+upgradoor,+upgradoor:team_*", "lighthouse"),
			by:            "team_hold", teams: []string{"lighthouse"},
		},
		{
			name: "team twin, several teams", action: "hold", body: holdMixed,
			authorization: provider.grant(t, "+upgradoor:team_hold", "teku", "geth"),
			by:            "team_hold", teams: []string{"geth", "teku"},
		},
		{
			name: "team twin, another team", action: "hold", body: holdMixed,
			authorization: provider.grant(t, "+upgradoor:team_hold", "teku"),
			refusal: "forbidden: upgradoor:team_hold does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "team twin, a token without teams", action: "hold", body: holdMixed,
			authorization: provider.grant(t, "+upgradoor:team_hold"),
			refusal: "forbidden: upgradoor:team_hold does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "roll of mixed ownership", action: "roll",
			body:          `{"hosts":["` + hostMixed + `"],"containers":["lighthouse","geth"]}`,
			authorization: provider.grant(t, "+upgradoor:team_roll", "lighthouse"),
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "another feature", action: "reboot", body: `{"hosts":["` + hostMixed + `"]}`,
			authorization: provider.grant(t, "+upgradoor:hold:roll", "lighthouse"),
			refusal:       "forbidden: this login lacks the feature upgradoor:reboot",
		},
		{
			name: "plain feature", action: "reboot", body: `{"hosts":["` + hostMixed + `"]}`,
			authorization: provider.grant(t, "+upgradoor:reboot", "teku"),
			by:            "reboot", teams: []string{"teku"},
		},
		{
			name: "force without admin", action: "reboot",
			body:          `{"hosts":["` + hostMixed + `"],"force":true}`,
			authorization: provider.grant(t, "+upgradoor:reboot"),
			refusal:       "forbidden: this login lacks the feature upgradoor:admin (needed for force)",
		},
		{
			name: "staff", action: "reboot", body: `{"all":true,"force":true}`,
			authorization: provider.grant(t, "+all:all", "panda"),
			by:            "reboot,admin", teams: []string{"panda"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := h.sched.recorded()
			rec := post(tt.action, tt.body, tt.authorization)
			calls := h.sched.recorded()

			if tt.refusal != "" {
				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.JSONEq(t, `{"error":`+string(mustJSON(t, tt.refusal))+`}`, rec.Body.String())
				assert.Empty(t, rec.Header().Get("WWW-Authenticate"), "403 is not a login problem")
				assert.Len(t, calls, len(before))

				return
			}

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Len(t, calls, len(before)+1)
			assert.Equal(t, userEmail, calls[len(before)].Actor)
			assert.Equal(t, scheduler.Authorization{By: tt.by, Teams: tt.teams}, calls[len(before)].Auth)
		})
	}

	// A token that may not use the service at all is no login here.
	rec := post("hold", holdMixed, provider.grant(t, "-all,+spamoor:all", "lighthouse"))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	require.NoError(t, err)

	return raw
}

// TestConfiguredServiceName: with --auth-service the token is checked for
// that service, for access and for features.
func TestConfiguredServiceName(t *testing.T) {
	c := &clock{now: testEpoch}
	provider := newAuthProvider(t, c, true)

	h := newHarness(t, func(o *Options) {
		o.AuthProviderURL = provider.srv.URL
		o.AuthService = "upgradoor-dev"
		o.Now = c.Now
	})
	h.clock = c
	h.start()
	ownershipFixture(h)

	hold := userRoute{
		method: http.MethodPost, path: "/api/v1/actions/hold", body: `{"hosts":["` + hostMixed + `"]}`,
	}
	read := userRoute{method: http.MethodGet, path: "/api/v1/hosts"}

	// Features of "upgradoor" are features of another service here.
	other := provider.grant(t, "+upgradoor:all")
	require.Equal(t, http.StatusOK, h.serve(read.request(other)).Code)

	rec := h.serve(hold.request(other))
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, ErrorBody{Error: "forbidden: this login lacks the feature upgradoor-dev:hold"},
		decodeJSON[ErrorBody](t, rec))

	require.Equal(t, http.StatusOK,
		h.serve(hold.request(provider.grant(t, "-all,+upgradoor-dev:hold"))).Code)
	require.Equal(t, http.StatusUnauthorized,
		h.serve(read.request(provider.grant(t, "-all,+upgradoor:all"))).Code)
}

// realHarness is a Server over the real store and the real scheduler, with an
// auth provider: what an action does is what it does in the controller.
func realHarness(t *testing.T) (*harness, *authProvider, store.Store) {
	t.Helper()

	log := logrus.New()
	log.SetOutput(io.Discard)

	st := store.NewMemory(log)
	require.NoError(t, st.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, st.Stop()) })

	c := &clock{now: testEpoch}
	provider := newAuthProvider(t, c, true)

	h := newHarness(t, func(o *Options) {
		o.AuthProviderURL = provider.srv.URL
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

	req := syncRequest()
	req.Containers = containers

	rec := h.agentPost(newAgent(t, identity.CertificateSpec{Hostname: host}), protocol.PathSync,
		req, agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestOwnersFromAgentToViews: what an agent reports as owners reaches the
// authenticated host views, cleaned, and no public view.
func TestOwnersFromAgentToViews(t *testing.T) {
	h, provider, _ := realHarness(t)

	const hidden = "qq-hidden-team"

	syncOwned(t, h, hostMixed,
		// Out of order, twice, and with names no team can have.
		owned("geth", "panda", hidden, "geth", "panda", "Not A Team", "geth:all", ""),
		protocol.Container{
			Name: "lighthouse", Image: "sigp/lighthouse:unstable", Digest: "sha256:c1", State: "running",
			Managed: true, Kind: protocol.KindCL, Owners: []string{"lighthouse"},
		},
		// Nine owners: one too many.
		owned("many", "i", "h", "g", "f", "e", "d", "c", "b", "a"),
		// Not managed: it has no owners, whatever the agent says.
		protocol.Container{Name: "debug", State: "running", Owners: []string{"lighthouse"}},
		owned("nobody"),
	)

	// An agent built before the field existed sends none.
	old := newAgent(t, identity.CertificateSpec{Hostname: hostUnowned})
	body := `{"protocolVersion":1,"agentVersion":"agent-0.9.0","sentAt":"2026-11-14T12:00:00Z",` +
		`"bootId":"boot-1","verbs":["roll"],"containers":[{"name":"bootnode","id":"1",` +
		`"image":"ethpandaops/bootnode:latest","imageId":"sha256:1","state":"running",` +
		`"restartCount":0,"startedAt":"2026-11-14T11:00:00Z","managed":true,"kind":"other"}]}`
	rec := h.serve(h.agentRequest(old, protocol.PathSync, []byte(body), agentCallOpts{}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	reader := provider.grant(t, "")

	owners := func(host HostSummary) map[string][]string {
		out := make(map[string][]string, len(host.Containers))
		for _, c := range host.Containers {
			out[c.Name] = c.Owners
		}

		return out
	}

	want := map[string][]string{
		"geth":       {"geth", "panda", hidden},
		"lighthouse": {"lighthouse"},
		"many":       {"a", "b", "c", "d", "e", "f", "g", "h"},
		"debug":      nil,
		"nobody":     nil,
	}

	rec = h.serve(userRoute{method: http.MethodGet, path: "/api/v1/hosts"}.request(reader))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	list := decodeJSON[Hosts](t, rec)
	require.Len(t, list.Hosts, 2)
	assert.Equal(t, hostUnowned, list.Hosts[0].Name)
	assert.Equal(t, map[string][]string{"bootnode": nil}, owners(list.Hosts[0]))
	assert.Equal(t, want, owners(list.Hosts[1]))
	assert.NotContains(t, rec.Body.String(), `"owners":[]`, "no owners is no field")
	assert.NotContains(t, rec.Body.String(), `"owners":null`)

	rec = h.serve(userRoute{
		method: http.MethodGet, path: "/api/v1/hosts/" + hostMixed,
	}.request(reader))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, want, owners(decodeJSON[HostDetail](t, rec).HostSummary))

	// The public views show the host and its images, and no owner.
	for _, path := range []string{"/api/v1/public/hosts", "/api/v1/public/images"} {
		rec := h.get(path)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), "sigp/lighthouse:unstable")
		assert.NotContains(t, rec.Body.String(), hidden, path)
		assert.NotContains(t, rec.Body.String(), "owners", path)
	}

	// Without a token the authenticated view is closed.
	require.Equal(t, http.StatusUnauthorized, h.get("/api/v1/hosts").Code)
}

// TestActionsEndToEnd runs actions through the real handlers, the real token
// verification, the real scheduler and the real store: a refusal changes
// nothing and writes no audit entry; what is allowed is recorded with the
// teams of the login and the feature that allowed it.
func TestActionsEndToEnd(t *testing.T) {
	h, provider, st := realHarness(t)
	ctx := context.Background()

	syncOwned(t, h, hostMixed, owned("geth", "geth"), owned("lighthouse", "lighthouse"))
	syncOwned(t, h, hostUnowned, owned("bootnode"))

	audit := func() []fleet.AuditEntry {
		entries, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action."})
		require.NoError(t, err)

		settings, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "settings."})
		require.NoError(t, err)

		return append(entries, settings...)
	}

	held := func(host string) bool {
		got, err := st.GetHost(ctx, host)
		require.NoError(t, err)

		return got.HoldWanted
	}

	send := func(method, path, body, authorization string) *httptest.ResponseRecorder {
		return h.serve(userRoute{method: method, path: path, body: body}.request(authorization))
	}

	hold := func(action, host, authorization string) *httptest.ResponseRecorder {
		return send(http.MethodPost, "/api/v1/actions/"+action,
			`{"hosts":["`+host+`"],"reason":"debugging"}`, authorization)
	}

	lighthouse := provider.grant(t, "-all,+upgradoor,+upgradoor:team_*", "lighthouse")
	teku := provider.grant(t, "-all,+upgradoor,+upgradoor:team_*", "teku")
	reader := provider.grant(t, "")

	require.Empty(t, audit())

	// Refused, three ways: nothing changes, nothing is written.
	for name, tt := range map[string]struct {
		authorization string
		host          string
		status        int
	}{
		"no token":              {"", hostMixed, http.StatusUnauthorized},
		"basic access":          {reader, hostMixed, http.StatusForbidden},
		"another team":          {teku, hostMixed, http.StatusForbidden},
		"a host nobody owns":    {lighthouse, hostUnowned, http.StatusForbidden},
		"a host nobody knows":   {lighthouse, hostGhost, http.StatusForbidden},
		"one of two is foreign": {lighthouse, hostMixed + `","` + hostUnowned, http.StatusForbidden},
	} {
		rec := hold("hold", tt.host, tt.authorization)
		require.Equal(t, tt.status, rec.Code, "%s: %s", name, rec.Body.String())
	}

	assert.False(t, held(hostMixed))
	assert.False(t, held(hostUnowned))
	assert.Empty(t, audit(), "a refusal is not audited")

	// Allowed through the team twin.
	rec := hold("hold", hostMixed, lighthouse)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, held(hostMixed))

	entries := audit()
	require.Len(t, entries, 1)
	assert.Equal(t, "action.hold", entries[0].Action)
	assert.Equal(t, userEmail, entries[0].Actor)
	assert.Equal(t, hostMixed, entries[0].Target)
	assert.Equal(t, "team_hold", entries[0].Detail["authorizedBy"])
	assert.Equal(t, []any{"lighthouse"}, entries[0].Detail["teams"])
	assert.Equal(t, "debugging", entries[0].Detail["reason"])
	assert.Contains(t, entries[0].Detail, "before")
	assert.Contains(t, entries[0].Detail, "after")

	// The other team cannot take the hold back.
	rec = hold("unhold", hostMixed, teku)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.True(t, held(hostMixed))
	assert.Len(t, audit(), 1)

	// A staff login can, and may name any host.
	staff := provider.grant(t, "+upgradoor:all", "panda")

	rec = hold("unhold", hostMixed, staff)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, held(hostMixed))

	entries = audit()
	require.Len(t, entries, 2)
	assert.Equal(t, "action.unhold", entries[0].Action)
	assert.Equal(t, "hold", entries[0].Detail["authorizedBy"])
	assert.Equal(t, []any{"panda"}, entries[0].Detail["teams"])

	// The global pause needs admin; with rollouts alone it stays as it was.
	rec = send(http.MethodPost, "/api/v1/actions/pause", `{}`, provider.grant(t, "+upgradoor:supervise"))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	settings := decodeJSON[SettingsBody](t, send(http.MethodGet, "/api/v1/settings", "", reader))
	assert.False(t, settings.Settings.Paused)

	rec = send(http.MethodPost, "/api/v1/actions/pause", `{"reason":"incident"}`,
		provider.grant(t, "+upgradoor:admin"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	entries = audit()
	require.Len(t, entries, 3)
	assert.Equal(t, "action.pause", entries[0].Action)
	assert.Equal(t, "admin", entries[0].Detail["authorizedBy"])
	assert.Equal(t, []any{}, entries[0].Detail["teams"], "a login without teams")

	// Settings: read by everyone, written with admin.
	changed := settings.Settings
	changed.Paused = true
	changed.MinPeers = 7

	body := string(mustJSON(t, changed))

	for _, authorization := range []string{reader, lighthouse, provider.grant(t, "+upgradoor:supervise:hold")} {
		rec = send(http.MethodPut, "/api/v1/settings", body, authorization)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, ErrorBody{Error: "forbidden: this login lacks the feature upgradoor:admin"},
			decodeJSON[ErrorBody](t, rec))
	}

	// Refused before the body is looked at.
	rec = send(http.MethodPut, "/api/v1/settings", `{"minPeers":`, reader)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	got := decodeJSON[SettingsBody](t, send(http.MethodGet, "/api/v1/settings", "", reader))
	assert.Equal(t, 3, got.Settings.MinPeers, "the default")
	assert.Len(t, audit(), 3)

	rec = send(http.MethodPut, "/api/v1/settings", body, provider.grant(t, "+upgradoor:admin", "panda"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	entries = audit()
	require.Len(t, entries, 4)
	assert.Equal(t, "settings.update", entries[3].Action)
	assert.Equal(t, "admin", entries[3].Detail["authorizedBy"])
	assert.Equal(t, []any{"panda"}, entries[3].Detail["teams"])
	assert.Contains(t, entries[3].Detail, "before")

	got = decodeJSON[SettingsBody](t, send(http.MethodGet, "/api/v1/settings", "", reader))
	assert.Equal(t, 7, got.Settings.MinPeers)

	// Reads are not audited.
	all, err := st.ListAudit(ctx, store.AuditFilter{})
	require.NoError(t, err)

	for _, e := range all {
		assert.NotContains(t, e.Action, "read")
	}
}
