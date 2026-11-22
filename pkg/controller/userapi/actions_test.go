package userapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestActions(t *testing.T) {
	tests := []struct {
		action string
		body   string
		want   any
	}{
		{
			action: "roll",
			body: `{"hosts":["lighthouse-geth-1","prysm-besu-1"],"containers":["lighthouse"],` +
				`"staggered":true,"force":true,"ignoreBudget":true,"reason":"hotfix"}`,
			want: scheduler.RollRequest{
				Hosts:      []string{"lighthouse-geth-1", "prysm-besu-1"},
				Containers: []string{"lighthouse"}, Staggered: true, Force: true,
				IgnoreBudget: true, Reason: "hotfix",
			},
		},
		{
			action: "roll", body: `{"all":true}`,
			want: scheduler.RollRequest{All: true},
		},
		{
			action: "reboot",
			body:   `{"all":true,"staggered":true,"reason":"kernel"}`,
			want:   scheduler.RebootRequest{All: true, Staggered: true, Reason: "kernel"},
		},
		{
			action: "reboot", body: `{"hosts":["bootnode-1"],"force":true}`,
			want: scheduler.RebootRequest{Hosts: []string{"bootnode-1"}, Force: true},
		},
		{
			action: "reboot", body: `{"hosts":["bootnode-1"],"staggered":true,"ignoreGate":true}`,
			want: scheduler.RebootRequest{Hosts: []string{"bootnode-1"}, Staggered: true, IgnoreGate: true},
		},
		{
			action: "hold", body: `{"hosts":["lighthouse-geth-1"],"reason":"debugging"}`,
			want: scheduler.HoldRequest{Hosts: []string{"lighthouse-geth-1"}, Reason: "debugging"},
		},
		{
			action: "unhold", body: `{"hosts":["lighthouse-geth-1"]}`,
			want: scheduler.HoldRequest{Hosts: []string{"lighthouse-geth-1"}},
		},
		{
			action: "forget", body: `{"hosts":["gone-1","gone-2"],"reason":"decommissioned"}`,
			want: scheduler.HoldRequest{
				Hosts: []string{"gone-1", "gone-2"}, Reason: "decommissioned",
			},
		},
		{
			action: "end-lease", body: `{"lease":"0123456789abcdef","reason":"host taken away"}`,
			want: scheduler.EndLeaseRequest{Lease: "0123456789abcdef", Reason: "host taken away"},
		},
		{
			action: "pause", body: `{"reason":"incident"}`,
			want: scheduler.ScopeRequest{Reason: "incident"},
		},
		{
			action: "pause", body: `{"rollout":"r-1"}`,
			want: scheduler.ScopeRequest{Rollout: "r-1"},
		},
		{
			action: "resume", body: `{}`,
			want: scheduler.ScopeRequest{},
		},
		{
			action: "resume", body: `{"rollout":"r-1","reason":"approved"}`,
			want: scheduler.ScopeRequest{Rollout: "r-1", Reason: "approved"},
		},
		{
			action: "abort", body: `{"rollout":"r-1","reason":"bad image"}`,
			want: scheduler.ScopeRequest{Rollout: "r-1", Reason: "bad image"},
		},
		{
			action: "skip", body: `{"rollout":"r-1","hosts":["prysm-besu-1"]}`,
			want: scheduler.RolloutHostsRequest{Rollout: "r-1", Hosts: []string{"prysm-besu-1"}},
		},
		{
			action: "retry", body: `{"rollout":"r-1"}`,
			want: scheduler.RolloutHostsRequest{Rollout: "r-1"},
		},
		{
			action: "retry", body: `{"rollout":"r-1","hosts":["prysm-besu-1"],"reason":"fixed"}`,
			want: scheduler.RolloutHostsRequest{
				Rollout: "r-1", Hosts: []string{"prysm-besu-1"}, Reason: "fixed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.action+" "+tt.body, func(t *testing.T) {
			h := newHarness(t, nil)

			result := &scheduler.ActionResult{
				Message: "queued 2 hosts", Rollout: "r-9",
				Hosts:    []string{"lighthouse-geth-1"},
				Rejected: map[string]string{"prysm-besu-1": "on hold"},
			}
			h.sched.Set(func(f *controllertest.Scheduler) { f.Result = result })

			rec := h.sendJSON(http.MethodPost, "/api/v1/actions/"+tt.action, tt.body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, *result, controllertest.DecodeJSON[scheduler.ActionResult](t, rec))

			calls := h.sched.Recorded()
			require.Len(t, calls, 1)
			assert.Equal(t, controllertest.Call{
				Name: tt.action, Actor: actorAnonymous, Req: tt.want, Auth: controllertest.OpenAuth,
			}, calls[0])
		})
	}
}

func TestActionValidation(t *testing.T) {
	longReason := strings.Repeat("r", maxReasonLength+1)

	manyHosts := make([]string, 0, maxActionHosts+1)
	for i := range maxActionHosts + 1 {
		manyHosts = append(manyHosts, fmt.Sprintf(`"h%d"`, i))
	}

	tests := []struct {
		action   string
		body     string
		contains string
	}{
		{action: "roll", body: `{}`, contains: "give hosts, or set all"},
		{action: "roll", body: `{"hosts":[]}`, contains: "give hosts, or set all"},
		{action: "roll", body: `{"hosts":["a"],"all":true}`, contains: "not both"},
		{action: "roll", body: `{"hosts":["../etc"]}`, contains: "invalid host name"},
		{action: "roll", body: `{"hosts":["a b"]}`, contains: "invalid host name"},
		{action: "roll", body: `{"hosts":[""]}`, contains: "invalid host name"},
		{action: "roll", body: `{"hosts":["a","a"]}`, contains: "listed twice"},
		{action: "roll", body: `{"all":true,"containers":[""]}`, contains: "invalid container"},
		{
			action: "roll", body: `{"all":true,"reason":"` + longReason + `"}`,
			contains: "reason is longer",
		},
		{
			action: "roll", body: `{"hosts":[` + strings.Join(manyHosts, ",") + `]}`,
			contains: "more than",
		},
		{action: "roll", body: `{"all":true,"hostz":["a"]}`, contains: "unknown field"},
		{action: "roll", body: `{"all":"yes"}`, contains: "invalid JSON"},
		{action: "roll", body: `[]`, contains: "invalid JSON"},
		{action: "roll", body: ``, contains: "invalid JSON"},
		{action: "roll", body: `{"all":true} {"all":true}`, contains: "trailing data"},
		{action: "reboot", body: `{}`, contains: "give hosts, or set all"},
		{action: "reboot", body: `{"hosts":["a"],"all":true}`, contains: "not both"},
		{action: "reboot", body: `{"all":true,"containers":["x"]}`, contains: "unknown field"},
		{action: "hold", body: `{}`, contains: "hosts must not be empty"},
		{action: "hold", body: `{"hosts":["a/b"]}`, contains: "invalid host name"},
		{action: "unhold", body: `{"hosts":[]}`, contains: "hosts must not be empty"},
		{action: "forget", body: `{"reason":"x"}`, contains: "hosts must not be empty"},
		{action: "forget", body: `{"hosts":["a"],"all":true}`, contains: "unknown field"},
		{action: "end-lease", body: `{"reason":"x"}`, contains: "lease must not be empty"},
		{action: "end-lease", body: `{"lease":"l","hosts":["a"]}`, contains: "unknown field"},
		{action: "end-lease", body: `{"lease":"l","reason":"` + longReason + `"}`, contains: "reason is longer"},
		{
			action: "pause", body: `{"reason":"` + longReason + `"}`,
			contains: "reason is longer",
		},
		{action: "pause", body: `{"hosts":["a"]}`, contains: "unknown field"},
		{
			action: "resume", body: `{"rollout":"` + strings.Repeat("x", maxNameLength+1) + `"}`,
			contains: "invalid rollout id",
		},
		{action: "abort", body: `{}`, contains: "rollout must not be empty"},
		{action: "abort", body: `{"reason":"x"}`, contains: "rollout must not be empty"},
		{action: "skip", body: `{"hosts":["a"]}`, contains: "rollout must not be empty"},
		{action: "skip", body: `{"rollout":"r-1"}`, contains: "hosts must not be empty"},
		{action: "skip", body: `{"rollout":"r-1","hosts":["!"]}`, contains: "invalid host name"},
		{action: "retry", body: `{}`, contains: "rollout must not be empty"},
		{action: "retry", body: `{"rollout":"r-1","hosts":["!"]}`, contains: "invalid host name"},
	}

	for _, tt := range tests {
		name := tt.action + " " + tt.body
		if len(name) > 60 {
			name = name[:60]
		}

		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)

			rec := h.sendJSON(http.MethodPost, "/api/v1/actions/"+tt.action, tt.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec).Error, tt.contains)
			assert.Empty(t, h.sched.Recorded(), "an invalid action never reaches the scheduler")
		})
	}
}

func TestActionErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{
			name:   "unknown host",
			err:    fmt.Errorf("%w: nope-1", scheduler.ErrUnknownHost),
			status: http.StatusNotFound, want: "scheduler: unknown host: nope-1",
		},
		{
			name:   "unknown rollout",
			err:    fmt.Errorf("%w: r-404", scheduler.ErrUnknownRollout),
			status: http.StatusNotFound, want: "scheduler: unknown rollout: r-404",
		},
		{
			name:   "unknown lease",
			err:    fmt.Errorf("%w: l-404", scheduler.ErrUnknownLease),
			status: http.StatusNotFound, want: "scheduler: unknown lease: l-404",
		},
		{
			name:   "invalid action",
			err:    fmt.Errorf("%w: host holds a lease", scheduler.ErrInvalidAction),
			status: http.StatusConflict, want: "scheduler: invalid action: host holds a lease",
		},
		{
			name:   "wrapped twice",
			err:    fmt.Errorf("forget: %w", fmt.Errorf("%w: lease open", scheduler.ErrInvalidAction)),
			status: http.StatusConflict, want: "forget: scheduler: invalid action: lease open",
		},
		{
			name:   "anything else is internal and not shown",
			err:    errors.New("secret: commit failed: database is locked"),
			status: http.StatusInternalServerError, want: "internal error",
		},
	}

	bodies := map[string]string{
		"roll": `{"all":true}`, "reboot": `{"all":true}`, "hold": `{"hosts":["a"]}`,
		"unhold": `{"hosts":["a"]}`, "forget": `{"hosts":["a"]}`, "pause": `{}`, "resume": `{}`,
		"abort": `{"rollout":"r"}`, "skip": `{"rollout":"r","hosts":["a"]}`,
		"retry": `{"rollout":"r"}`, "end-lease": `{"lease":"l"}`,
	}

	for _, tt := range tests {
		for action, body := range bodies {
			t.Run(tt.name+"/"+action, func(t *testing.T) {
				h := newHarness(t, nil)
				h.sched.Set(func(f *controllertest.Scheduler) { f.ActionErr = tt.err })

				rec := h.sendJSON(http.MethodPost, "/api/v1/actions/"+action, body)
				require.Equal(t, tt.status, rec.Code, rec.Body.String())
				assert.Equal(t, httpjson.ErrorBody{Error: tt.want}, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))
			})
		}
	}
}

func TestActionRequestShape(t *testing.T) {
	h := newHarness(t, nil)

	// Unknown actions and wrong methods: the server's TestUnknownRoutes.
	var rec *httptest.ResponseRecorder

	// A body that is not declared as JSON (what an HTML form can send).
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/actions/roll",
			strings.NewReader(`{"all":true}`))

		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		rec = h.serve(req)
		require.Equal(t, http.StatusUnsupportedMediaType, rec.Code, contentType)
	}

	// A body over the limit.
	rec = h.sendJSON(http.MethodPost, "/api/v1/actions/roll",
		`{"all":true,"reason":"`+strings.Repeat("a", maxUserBody)+`"}`)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)

	assert.Empty(t, h.sched.Recorded())

	// A scheduler that answers nothing still yields a result.
	h.sched.Set(func(f *controllertest.Scheduler) { f.Result = nil })

	rec = h.sendJSON(http.MethodPost, "/api/v1/actions/pause", `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, scheduler.ActionResult{Message: "ok"},
		controllertest.DecodeJSON[scheduler.ActionResult](t, rec))
}

// TestActionsEndToEnd runs actions through the real handlers, the real token
// verification, the real scheduler and the real store: a refusal changes
// nothing and writes no audit entry; what is allowed is recorded with the
// teams of the login and the feature that allowed it.
func TestActionsEndToEnd(t *testing.T) {
	h, provider, st := realHarness(t)
	ctx := context.Background()

	syncOwned(t, st, controllertest.HostMixed, controllertest.Owned("geth", protocol.KindEL, "geth"), controllertest.Owned("lighthouse", protocol.KindCL, "lighthouse"))
	syncOwned(t, st, controllertest.HostUnowned, controllertest.Owned("bootnode", "bootnode", ""))

	audit := func() []fleet.AuditEntry {
		entries, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action."})
		require.NoError(t, err)

		settings, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "settings."})
		require.NoError(t, err)

		return append(entries, settings...)
	}

	// requested is the hold and release-hold directives that wait for the host.
	requested := func(host string) []protocol.DirectiveKind {
		pending, err := st.ListDirectives(ctx, store.DirectiveFilter{Host: host, PendingOnly: true})
		require.NoError(t, err)

		kinds := make([]protocol.DirectiveKind, 0, len(pending))
		for _, d := range pending {
			kinds = append(kinds, d.Kind)
		}

		return kinds
	}

	send := func(method, path, body, authorization string) *httptest.ResponseRecorder {
		return h.serve(userRoute{method: method, path: path, body: body}.request(authorization))
	}

	hold := func(action, host, authorization string) *httptest.ResponseRecorder {
		return send(http.MethodPost, "/api/v1/actions/"+action,
			`{"hosts":["`+host+`"],"reason":"debugging"}`, authorization)
	}

	lighthouse := provider.Grant(t, "-all,+upgradoor,+upgradoor:team_*", "lighthouse")
	teku := provider.Grant(t, "-all,+upgradoor,+upgradoor:team_*", "teku")
	reader := provider.Grant(t, "")

	require.Empty(t, audit())

	// Refused, three ways: nothing changes, nothing is written.
	for name, tt := range map[string]struct {
		authorization string
		host          string
		status        int
	}{
		"no token":              {"", controllertest.HostMixed, http.StatusUnauthorized},
		"basic access":          {reader, controllertest.HostMixed, http.StatusForbidden},
		"another team":          {teku, controllertest.HostMixed, http.StatusForbidden},
		"a host nobody owns":    {lighthouse, controllertest.HostUnowned, http.StatusForbidden},
		"a host nobody knows":   {lighthouse, controllertest.HostGhost, http.StatusForbidden},
		"one of two is foreign": {lighthouse, controllertest.HostMixed + `","` + controllertest.HostUnowned, http.StatusForbidden},
	} {
		rec := hold("hold", tt.host, tt.authorization)
		require.Equal(t, tt.status, rec.Code, "%s: %s", name, rec.Body.String())
	}

	assert.Empty(t, requested(controllertest.HostMixed))
	assert.Empty(t, requested(controllertest.HostUnowned))
	assert.Empty(t, audit(), "a refusal is not audited")

	// Allowed through the team twin.
	rec := hold("hold", controllertest.HostMixed, lighthouse)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []protocol.DirectiveKind{protocol.DirectiveHold}, requested(controllertest.HostMixed),
		"the hold waits for the host")

	entries := audit()
	require.Len(t, entries, 1)
	assert.Equal(t, "action.hold", entries[0].Action)
	assert.Equal(t, controllertest.UserEmail, entries[0].Actor)
	assert.Equal(t, controllertest.HostMixed, entries[0].Target)
	assert.Equal(t, "team_manage", entries[0].Detail["authorizedBy"])
	assert.Equal(t, []any{"lighthouse"}, entries[0].Detail["teams"])
	assert.Equal(t, "debugging", entries[0].Detail["reason"])
	assert.Contains(t, entries[0].Detail, "before")
	assert.Contains(t, entries[0].Detail, "after")

	// The other team cannot take the hold back.
	rec = hold("unhold", controllertest.HostMixed, teku)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, []protocol.DirectiveKind{protocol.DirectiveHold}, requested(controllertest.HostMixed))
	assert.Len(t, audit(), 1)

	// A staff login can, naming the team's hold.
	staff := provider.Grant(t, "+upgradoor:all", "panda")

	rec = send(http.MethodPost, "/api/v1/actions/unhold",
		`{"hosts":["`+controllertest.HostMixed+`"],"holders":["lighthouse"]}`, staff)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []protocol.DirectiveKind{protocol.DirectiveReleaseHold}, requested(controllertest.HostMixed),
		"the release replaces the unanswered hold")

	entries = audit()
	require.Len(t, entries, 2)
	assert.Equal(t, "action.unhold", entries[0].Action)
	assert.Equal(t, "manage", entries[0].Detail["authorizedBy"])
	assert.Equal(t, []any{"panda"}, entries[0].Detail["teams"])

	// The global pause needs admin; with rollouts alone it stays as it was.
	rec = send(http.MethodPost, "/api/v1/actions/pause", `{}`, provider.Grant(t, "+upgradoor:manage"))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	admin := provider.Grant(t, "+upgradoor:admin")

	settings := controllertest.DecodeJSON[views.SettingsBody](t, send(http.MethodGet, "/api/v1/settings", "", admin))
	assert.False(t, settings.Settings.Paused)

	rec = send(http.MethodPost, "/api/v1/actions/pause", `{"reason":"incident"}`,
		admin)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	entries = audit()
	require.Len(t, entries, 3)
	assert.Equal(t, "action.pause", entries[0].Action)
	assert.Equal(t, "admin", entries[0].Detail["authorizedBy"])
	assert.Equal(t, []any{}, entries[0].Detail["teams"], "a login without teams")

	// Settings: read and written with admin.
	changed := settings.Settings
	changed.Paused = true
	changed.MinPeers = 7

	body := string(controllertest.MustJSON(t, changed))

	for _, authorization := range []string{reader, lighthouse, provider.Grant(t, "+upgradoor:manage")} {
		rec = send(http.MethodPut, "/api/v1/settings", body, authorization)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, httpjson.ErrorBody{Error: "forbidden: this login lacks the feature upgradoor:admin"},
			controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))
	}

	// Refused before the body is looked at.
	rec = send(http.MethodPut, "/api/v1/settings", `{"minPeers":`, reader)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	for _, authorization := range []string{reader, lighthouse, provider.Grant(t, "+upgradoor:details,manage*")} {
		rec = send(http.MethodGet, "/api/v1/settings", "", authorization)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, httpjson.ErrorBody{Error: "forbidden: this login lacks the feature upgradoor:admin"},
			controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))
	}

	got := controllertest.DecodeJSON[views.SettingsBody](t, send(http.MethodGet, "/api/v1/settings", "", admin))
	assert.Equal(t, 3, got.Settings.MinPeers, "the default")
	assert.Len(t, audit(), 3)

	rec = send(http.MethodPut, "/api/v1/settings", body, provider.Grant(t, "+upgradoor:admin", "panda"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	entries = audit()
	require.Len(t, entries, 4)
	assert.Equal(t, "settings.update", entries[3].Action)
	assert.Equal(t, "admin", entries[3].Detail["authorizedBy"])
	assert.Equal(t, []any{"panda"}, entries[3].Detail["teams"])
	assert.Contains(t, entries[3].Detail, "before")

	got = controllertest.DecodeJSON[views.SettingsBody](t, send(http.MethodGet, "/api/v1/settings", "", admin))
	assert.Equal(t, 7, got.Settings.MinPeers)

	// Reads are not audited.
	all, err := st.ListAudit(ctx, store.AuditFilter{})
	require.NoError(t, err)

	for _, e := range all {
		assert.NotContains(t, e.Action, "read")
	}
}

// TestRolloutActionRequirements: a retry with hosts recovers hosts, whatever
// the rollout's state (an aborted one is refused by the scheduler); pause,
// resume (approve), abort and retry of a rollout are the team's of its
// client.
func TestRolloutActionRequirements(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	retry := fmt.Sprintf(`{"rollout":"r-aborted","hosts":[%q]}`, controllertest.HostMixed)
	lighthouse := []string{"lighthouse"}

	for _, tt := range []authzCase{
		{
			name: "retry hosts of an aborted rollout, the team twin", action: "retry", body: retry,
			services: "+upgradoor:team_manage", teams: lighthouse, by: "team_manage",
		},
		{
			name: "retry an aborted rollout of another team", action: "retry", body: `{"rollout":"r-aborted"}`,
			services: "+upgradoor:team_manage", teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_manage does not cover rollout r-aborted (owners: lighthouse)",
		},
		{
			name: "approve the team's client", action: "resume", body: `{"rollout":"r-lighthouse"}`,
			services: "+upgradoor:team_manage", teams: lighthouse, by: "team_manage",
		},
		{
			name: "approve another team's client", action: "resume", body: `{"rollout":"r-lighthouse"}`,
			services: "+upgradoor:team_manage", teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_manage does not cover rollout r-lighthouse (owners: lighthouse)",
		},
		{
			name: "pause a rollout for no team", action: "pause", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:team_manage", teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover rollout r-1: it is for no team",
		},
		{
			name: "pause an unknown rollout", action: "pause", body: `{"rollout":"r-ghost"}`,
			services: "+upgradoor:team_manage", teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover rollout r-ghost (owners: none)",
		},
		{
			name: "abort the team's rollout", action: "abort", body: `{"rollout":"r-lighthouse"}`,
			services: "+upgradoor:team_*", teams: lighthouse, by: "team_manage",
		},
		{
			name: "retry the team's rollout", action: "retry", body: `{"rollout":"r-lighthouse"}`,
			services: "+upgradoor:team_manage", teams: lighthouse, by: "team_manage",
		},
		{
			name: "abort a rollout for no team", action: "abort", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:team_*", teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover rollout r-1: it is for no team",
		},
		{
			name: "skip a member with manage", action: "skip",
			body:     fmt.Sprintf(`{"rollout":"r-1","hosts":[%q]}`, controllertest.HostMixed),
			services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "skip a member of the team's roll with team_manage", action: "skip",
			body:     fmt.Sprintf(`{"rollout":"r-aborted","hosts":[%q]}`, controllertest.HostMixed),
			services: "+upgradoor:team_manage", teams: lighthouse, by: "team_manage",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}

	// When the rollout cannot be read the request fails; it is not decided.
	h.store.SetErr(assert.AnError)

	req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/x",
		strings.NewReader(`{"rollout":"r-lighthouse"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.srv.actions()["resume"](rec, req, &authz.User{
		Actor: controllertest.UserEmail, Services: "+upgradoor:team_manage", Teams: lighthouse,
	})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestRefusalsAreAudited: a refusal is audited once per actor and path in a
// window, so refusals cannot flood the log.
func TestRefusalsAreAudited(t *testing.T) {
	h := newHarness(t, nil)

	refusals := func() []fleet.AuditEntry {
		return slices.DeleteFunc(h.store.AuditEntries(), func(e fleet.AuditEntry) bool {
			return e.Action != "auth.refused"
		})
	}

	send := func(actor, path string) {
		req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		h.srv.actions()["forget"](rec, req, &authz.User{Actor: actor, Teams: []string{"geth"}})
		require.Equal(t, http.StatusForbidden, rec.Code)
	}

	send("eve@example.com", "/api/v1/actions/forget")
	send("eve@example.com", "/api/v1/actions/forget")
	send("mallory@example.com", "/api/v1/actions/forget")

	got := refusals()
	require.Len(t, got, 2)
	assert.Equal(t, "eve@example.com", got[0].Actor)
	assert.Equal(t, "/api/v1/actions/forget", got[0].Target)
	assert.Equal(t, "forbidden: this login lacks the feature upgradoor:admin", got[0].Detail["reason"])
	assert.Equal(t, []string{"geth"}, got[0].Detail["teams"])

	h.clock.Advance(time.Minute)
	send("eve@example.com", "/api/v1/actions/forget")
	assert.Len(t, refusals(), 3, "audited again after the window")
}

// TestTeamHoldTouchesOnlyItsTeamsHolds: team_manage sets and lifts the holds
// of the token's teams that own a container of the host; an unhold that
// names staff's or another team's hold is refused. The plain feature lifts
// any hold it names.
func TestTeamHoldTouchesOnlyItsTeamsHolds(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	h.store.Lock()
	h.store.Hosts[controllertest.HostMixed].Status.Hold.Holds = []protocol.HostHold{{Holder: fleet.HolderStaff, Reason: "incident"}}
	h.store.Hosts[controllertest.HostTeku].Status.Hold.Holds = []protocol.HostHold{{Holder: "geth"}, {Holder: "teku"}}
	h.store.Unlock()

	body := func(holders string, names ...string) string {
		return `{"hosts":["` + strings.Join(names, `","`) + `"]` + holders + `}`
	}
	twin := "+upgradoor:team_manage"

	for _, tt := range []authzCase{
		{
			name: "unhold the team's holds", action: "unhold", body: body("", controllertest.HostMixed),
			services: twin, teams: []string{"lighthouse"}, by: "team_manage",
		},
		{
			name: "unhold staff's hold", action: "unhold", body: body(`,"holders":["staff"]`, controllertest.HostMixed),
			services: "+upgradoor:team_*", teams: []string{"geth", "lighthouse"},
			refusal: "forbidden: upgradoor:team_manage does not lift the hold of staff on host lighthouse-geth-1",
		},
		{
			name: "unhold the team's hold by name", action: "unhold", body: body(`,"holders":["teku"]`, controllertest.HostTeku),
			services: twin, teams: []string{"teku"}, by: "team_manage",
		},
		{
			name: "unhold another team's hold", action: "unhold", body: body(`,"holders":["teku"]`, controllertest.HostTeku),
			services: twin, teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_manage does not lift the hold of teku on host teku-geth-1",
		},
		{
			name: "hold, a team without a container there", action: "hold", body: body("", controllertest.HostTeku),
			services: twin, teams: []string{"lighthouse"},
			refusal: "forbidden: upgradoor:team_manage does not cover host teku-geth-1 (owners: geth, teku)",
		},
		{
			name: "hold where staff holds", action: "hold", body: body("", controllertest.HostMixed),
			services: twin, teams: []string{"lighthouse"}, by: "team_manage",
		},
		{
			name: "a host nobody owns", action: "unhold", body: body("", controllertest.HostUnowned),
			services: twin, teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_manage does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "the plain feature lifts any hold it names", action: "unhold",
			body:     body(`,"holders":["staff","teku"]`, controllertest.HostTeku, controllertest.HostMixed),
			services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "mark offline through the twin", action: "mark-offline", body: `{"hosts":["teku-geth-1"],"reason":"x"}`,
			services: twin, teams: []string{"teku"},
			refusal: "forbidden: this login lacks the feature upgradoor:manage",
		},
		{
			name: "unmark offline through the twin", action: "unmark-offline", body: body("", controllertest.HostTeku),
			services: twin, teams: []string{"teku"},
			refusal: "forbidden: this login lacks the feature upgradoor:manage",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}

	// The host views list the holders, the details every hold.
	list := controllertest.DecodeJSON[views.Hosts](t, h.get("/api/v1/hosts"))
	holders := make(map[string][]string, len(list.Hosts))

	for _, host := range list.Hosts {
		holders[host.Name] = host.Holders
	}

	assert.Equal(t, map[string][]string{
		controllertest.HostMixed: {fleet.HolderStaff}, controllertest.HostTeku: {"geth", "teku"},
		controllertest.HostUnowned: {}, controllertest.HostFresh: {},
	}, holders)
	assert.Equal(t, []protocol.HostHold{{Holder: fleet.HolderStaff, Reason: "incident"}},
		controllertest.DecodeJSON[views.HostDetail](t, h.get("/api/v1/hosts/"+controllertest.HostMixed)).Hold.Holds)
	assert.NotContains(t, h.get("/api/v1/public/hosts").Body.String(), "holder")
}

// TestRollFlagsReachTheScheduler: a roll "now", force, ignoreBudget and all
// are what manage and its twin ask for; nothing is taken out of the request.
func TestRollFlagsReachTheScheduler(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	roll := `{"hosts":["` + controllertest.HostMixed + `"],"containers":["lighthouse"],"force":true,"ignoreBudget":true}`
	reboot := `{"hosts":["` + controllertest.HostMixed + `"],"force":true}`

	for _, tt := range []struct {
		name     string
		action   string
		body     string
		services string
	}{
		{"roll, team twin", "roll", roll, "+upgradoor:team_manage"},
		{"roll, plain feature", "roll", roll, "+upgradoor:manage"},
		{"roll, admin", "roll", roll, "+upgradoor:admin"},
		{"reboot, plain feature", "reboot", reboot, "+upgradoor:manage"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(h.sched.Recorded())

			req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/x", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")

			rec := httptest.NewRecorder()
			h.srv.actions()[tt.action](rec, req, &authz.User{
				Actor: controllertest.UserEmail, Services: tt.services, Teams: []string{"lighthouse"},
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			calls := h.sched.Recorded()
			require.Len(t, calls, before+1)

			switch got := calls[before].Req.(type) {
			case scheduler.RollRequest:
				assert.False(t, got.Staggered)
				assert.True(t, got.Force)
				assert.True(t, got.IgnoreBudget)
			case scheduler.RebootRequest:
				assert.False(t, got.Staggered)
				assert.True(t, got.Force)
			default:
				require.Fail(t, "unexpected request", "%T", got)
			}
		})
	}
}

// TestOwnershipIsExact: no teams own nothing, also where nobody owns
// anything; and a team is its name, byte for byte.
func TestOwnershipIsExact(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	hosts := func(name string) string { return `{"hosts":["` + name + `"]}` }

	for _, tt := range []authzCase{
		{
			name: "no teams, a host nobody owns", action: "hold", body: hosts(controllertest.HostUnowned),
			services: "+upgradoor:team_manage",
			refusal:  "forbidden: upgradoor:team_manage does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "an empty team list, a host nobody owns", action: "hold", body: hosts(controllertest.HostUnowned),
			services: "+upgradoor:team_manage", teams: []string{},
			refusal: "forbidden: upgradoor:team_manage does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "no teams, roll a host nobody owns", action: "roll", body: hosts(controllertest.HostUnowned),
			services: "+upgradoor:team_manage",
			refusal: "forbidden: upgradoor:team_manage does not cover container bootnode " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "no teams, roll a container nobody owns", action: "roll",
			body:     `{"hosts":["` + controllertest.HostUnowned + `"],"containers":["bootnode"]}`,
			services: "+upgradoor:team_manage",
			refusal: "forbidden: upgradoor:team_manage does not cover container bootnode " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "no teams, a host that never synced", action: "hold", body: hosts(controllertest.HostFresh),
			services: "+upgradoor:team_manage",
			refusal:  "forbidden: upgradoor:team_manage does not cover host fresh-1 (owners: none)",
		},
		{
			name: "an empty team name", action: "hold", body: hosts(controllertest.HostUnowned),
			services: "+upgradoor:team_manage", teams: []string{""},
			refusal: "forbidden: upgradoor:team_manage does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "another case", action: "hold", body: hosts(controllertest.HostMixed),
			services: "+upgradoor:team_manage", teams: []string{"Lighthouse", "GETH"},
			refusal: "forbidden: upgradoor:team_manage does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "another case, a roll", action: "roll",
			body:     `{"hosts":["` + controllertest.HostMixed + `"],"containers":["lighthouse"]}`,
			services: "+upgradoor:team_manage", teams: []string{"Lighthouse"},
			refusal: "forbidden: upgradoor:team_manage does not cover container lighthouse " +
				"on host lighthouse-geth-1 (owners: lighthouse)",
		},
		{
			name: "a prefix of the team", action: "hold", body: hosts(controllertest.HostMixed),
			services: "+upgradoor:team_manage", teams: []string{"light", "lighthouse2", " lighthouse"},
			refusal: "forbidden: upgradoor:team_manage does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "the name", action: "hold", body: hosts(controllertest.HostMixed),
			services: "+upgradoor:team_manage", teams: []string{"Lighthouse", "lighthouse"},
			by: "team_manage",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}
}

// TestRefusalsEchoSafely: a name a refusal repeats is cut to 128 characters
// and carries printable ASCII only, whether it came with the request or from
// an agent.
func TestRefusalsEchoSafely(t *testing.T) {
	assert.Equal(t, "lighthouse-geth-1", authz.Echo("lighthouse-geth-1"))
	assert.Equal(t, "a?b??c?", authz.Echo("a\nb\x1b\x00c\x7f"))
	assert.Equal(t, "h??st", authz.Echo("hößt"[:1]+"\xc3\xb6st"), "bytes outside ASCII")
	assert.Equal(t, strings.Repeat("x", 128), authz.Echo(strings.Repeat("x", 500)))
	assert.Empty(t, authz.Echo(""))

	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	// What an agent reported, stored as it came (the sync cleans owners, not
	// container names).
	h.store.Lock()
	h.store.Hosts[controllertest.HostUnowned].Status.Containers = append(h.store.Hosts[controllertest.HostUnowned].Status.Containers,
		protocol.Container{Name: "evil\r\n\x1b[31m" + strings.Repeat("n", 300), Managed: true})
	h.store.Unlock()

	refusal := func(action, body string) string {
		req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/x", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		h.srv.actions()[action](rec, req, &authz.User{
			Actor: controllertest.UserEmail, Services: "+upgradoor:team_*", Teams: []string{"geth"},
		})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		return controllertest.DecodeJSON[httpjson.ErrorBody](t, rec).Error
	}

	long := strings.Repeat("h", 4000)

	for name, got := range map[string]string{
		"host":      refusal("hold", `{"hosts":["bad\u0007\u001b[2Jhosté`+long+`"]}`),
		"container": refusal("roll", `{"hosts":["`+controllertest.HostMixed+`"],"containers":["c\n\u0000`+long+`"]}`),
		"lease":     refusal("end-lease", `{"lease":"l\t`+long+`"}`),
		"rollout":   refusal("pause", `{"rollout":"r\u001b`+strings.Repeat("r", 200)+`"}`),
		"agent":     refusal("roll", `{"hosts":["`+controllertest.HostUnowned+`"]}`),
	} {
		assert.Less(t, len(got), 400, name)

		for _, c := range []byte(got) {
			require.True(t, c >= 0x20 && c <= 0x7e, "%s: byte %#x in %q", name, c, got)
		}
	}

	assert.Equal(t, "forbidden: upgradoor:team_manage does not cover host bad??[2Jhost??"+
		strings.Repeat("h", 128-len("bad??[2Jhost??"))+" (owners: none)",
		refusal("hold", `{"hosts":["bad\u0007\u001b[2Jhosté`+long+`"]}`))
}

// TestDuplicateContainerNames: a host that reports one name twice does not
// get to pick the owners afterwards. The first entry is the container; the
// later ones are dropped when the sync is stored (agentapi), and the
// ownership is that of the stored entry.
func TestDuplicateContainerNames(t *testing.T) {
	h := newHarness(t, nil)

	req := controllertest.SyncRequest()
	req.Containers = []protocol.Container{
		controllertest.Owned("lighthouse", protocol.KindCL, "lighthouse"),
		controllertest.Owned("geth", protocol.KindEL, "geth"),
		// The same name again, for another team, and once more unmanaged.
		controllertest.Owned("lighthouse", protocol.KindCL, "teku"),
		{Name: "lighthouse", State: "exited"},
		controllertest.Owned("geth", protocol.KindEL, ""),
	}

	// What the agent API stores of it (its TestDuplicateContainerNames).
	req.Containers = req.Containers[:2]

	h.store.Lock()
	h.store.Hosts[controllertest.HostMixed] = &fleet.Host{
		Name: controllertest.HostMixed, LastSeen: controllertest.Epoch, Status: req,
	}
	h.store.Unlock()

	roll := `{"hosts":["` + controllertest.HostMixed + `"],"containers":["lighthouse"]}`

	for _, tt := range []authzCase{
		{
			name: "the team of the second entry", action: "roll", body: roll,
			services: "+upgradoor:team_manage", teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_manage does not cover container lighthouse " +
				"on host lighthouse-geth-1 (owners: lighthouse)",
		},
		{
			name: "the team of the second entry, host-level", action: "hold",
			body:     `{"hosts":["` + controllertest.HostMixed + `"]}`,
			services: "+upgradoor:team_manage", teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_manage does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "the team of the first entry", action: "roll", body: roll,
			services: "+upgradoor:team_manage", teams: []string{"lighthouse"}, by: "team_manage",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}

	// The view shows what was stored.
	detail := controllertest.DecodeJSON[views.HostDetail](t, h.get("/api/v1/hosts/"+controllertest.HostMixed))
	require.Len(t, detail.Containers, 2)
}

// TestTokenWithTooManyTeams: the teams of a token are decided with and are
// written into the audit entries of its actions. A token with more than 64
// is not a login; with 64 it is.
func TestTokenWithTooManyTeams(t *testing.T) {
	h, _ := authHarness(t, true)
	seedHost(h)

	teams := func(n int) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, fmt.Sprintf("team-%03d", i))
		}

		return out
	}

	with := func(n int) {
		h.srv.users.mu.Lock()
		h.srv.users.verifier = teamsVerifier{teams: teams(n)}
		h.srv.users.mu.Unlock()
	}

	read := userRoute{method: http.MethodGet, path: "/api/v1/hosts"}
	pause := userRoute{method: http.MethodPost, path: "/api/v1/actions/pause", body: `{}`}

	with(64)
	require.Equal(t, http.StatusOK, h.serve(read.request("Bearer x")).Code)
	require.Equal(t, http.StatusOK, h.serve(pause.request("Bearer x")).Code)

	calls := h.sched.Recorded()
	require.Len(t, calls, 1)
	assert.Len(t, calls[0].Auth.Teams, 64)

	with(65)

	for _, route := range []userRoute{read, pause} {
		rec := h.serve(route.request("Bearer x"))
		require.Equal(t, http.StatusUnauthorized, rec.Code, route.path)
		assert.Equal(t, httpjson.ErrorBody{Error: errUnauthenticated.Error()}, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec),
			"the answer does not say why")
	}

	assert.Len(t, h.sched.Recorded(), 1)
}

// TestRetryAndHoldEndToEnd runs the cases of the review through the real
// handlers, token verification, scheduler and store.
func TestRetryAndHoldEndToEnd(t *testing.T) {
	h, provider, st := realHarness(t)
	ctx := context.Background()

	syncOwned(t, st, controllertest.HostMixed, controllertest.Owned("geth", protocol.KindEL, "geth"), controllertest.Owned("lighthouse", protocol.KindCL, "lighthouse"))
	syncOwned(t, st, controllertest.HostTeku, controllertest.Owned("geth", protocol.KindEL, "geth"), controllertest.Owned("teku", protocol.KindCL, "teku"))
	syncOwned(t, st, controllertest.HostUnowned, controllertest.Owned("bootnode", "bootnode", ""))

	// Aborted: a rollout of teku's containers.
	aborted := controllertest.RolloutRow("r-aborted", fleet.RolloutAborted, map[string]fleet.HostState{
		controllertest.HostTeku: fleet.HostPending,
	})
	aborted.Kind, aborted.Team = fleet.RolloutGroup, "teku"
	// Halted: a manual roll of lighthouse and teku failed on their hosts; the
	// bootnode waits.
	halted := controllertest.RolloutRow("r-halted", fleet.RolloutHalted, map[string]fleet.HostState{
		controllertest.HostMixed: fleet.HostFailed, controllertest.HostTeku: fleet.HostFailed, controllertest.HostUnowned: fleet.HostPending,
	})
	halted.Kind, halted.Key = fleet.RolloutManual, "manual:r-halted"

	directive := func(id, host, container string) *fleet.Directive {
		return &fleet.Directive{
			Directive: protocol.Directive{ID: id, Kind: protocol.DirectiveRoll, Container: container},
			Host:      host, State: fleet.DirectiveAccepted, RolloutID: "r-halted",
			CreatedBy: "alice", CreatedAt: controllertest.Epoch, UpdatedAt: controllertest.Epoch,
		}
	}

	require.NoError(t, st.Commit(ctx, &store.Changes{
		Rollouts: []*fleet.Rollout{aborted, halted},
		Directives: []*fleet.Directive{
			directive("d-1", controllertest.HostMixed, "lighthouse"),
			directive("d-2", controllertest.HostTeku, "teku"),
			directive("d-3", controllertest.HostUnowned, "bootnode"),
		},
	}))

	send := func(action, body, authorization string) (int, string) {
		rec := h.serve(userRoute{
			method: http.MethodPost, path: "/api/v1/actions/" + action, body: body,
		}.request(authorization))

		return rec.Code, controllertest.DecodeJSON[map[string]any](t, rec)["error"].(string) //nolint:errcheck // "" when there is none.
	}

	ok := func(action, body, authorization string) scheduler.ActionResult {
		t.Helper()

		rec := h.serve(userRoute{
			method: http.MethodPost, path: "/api/v1/actions/" + action, body: body,
		}.request(authorization))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		return controllertest.DecodeJSON[scheduler.ActionResult](t, rec)
	}

	rollout := func(id string) *fleet.Rollout {
		r, err := st.GetRollout(ctx, id)
		require.NoError(t, err)

		return r
	}

	actions := func() int {
		entries, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action."})
		require.NoError(t, err)

		return len(entries)
	}

	open := func() int {
		rollouts, err := st.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true})
		require.NoError(t, err)

		return len(rollouts)
	}

	lighthouse := provider.Grant(t, "-all,+upgradoor,+upgradoor:team_manage", "lighthouse")
	teku := provider.Grant(t, "-all,+upgradoor,+upgradoor:team_*", "teku")
	supervisor := provider.Grant(t, "+upgradoor:manage")
	staff := provider.Grant(t, controllertest.StaffServices, "panda")

	retry := func(id string, hosts ...string) string {
		if len(hosts) == 0 {
			return `{"rollout":"` + id + `"}`
		}

		return `{"rollout":"` + id + `","hosts":["` + strings.Join(hosts, `","`) + `"]}`
	}

	// An abort is final: nobody retries an aborted rollout.
	code, msg := send("retry", retry("r-aborted", controllertest.HostTeku), teku)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "scheduler: invalid action: rollout r-aborted is aborted, which is final", msg)

	code, msg = send("retry", retry("r-aborted"), supervisor)
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, msg, "rollout r-aborted is aborted")

	assert.Equal(t, 1, open(), "nothing was reopened")
	assert.Zero(t, actions())

	// A covered host that did not fail lifts no halt.
	code, msg = send("retry", retry("r-halted", controllertest.HostUnowned), staff)
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, msg, "no host could be retried: bootnode-1 has neither failed, been cancelled nor been skipped")

	// A team retries its own failed host: the other team's keeps the halt.
	res := ok("retry", retry("r-halted", controllertest.HostMixed), lighthouse)
	assert.Equal(t, []string{controllertest.HostMixed}, res.Hosts)
	assert.Contains(t, res.Message, "the rollout stays halted: teku-geth-1 still failed")
	assert.Equal(t, fleet.RolloutHalted, rollout("r-halted").State)
	assert.Equal(t, fleet.HostPending, rollout("r-halted").Host(controllertest.HostMixed).State)

	// Again: it is not failed any more, so nothing changes.
	code, _ = send("retry", retry("r-halted", controllertest.HostMixed), lighthouse)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, fleet.RolloutHalted, rollout("r-halted").State)

	// The last failed host: the halt is lifted.
	res = ok("retry", retry("r-halted", controllertest.HostTeku), teku)
	assert.Equal(t, []string{controllertest.HostTeku}, res.Hosts)
	assert.Equal(t, fleet.RolloutActive, rollout("r-halted").State)
	assert.Equal(t, 2, actions())

	// Holds. A team's hold: the team lifts it.
	ok("hold", `{"hosts":["`+controllertest.HostTeku+`"],"reason":"ours"}`, teku)
	obeyHolds(t, st, controllertest.HostTeku)

	host, err := st.GetHost(ctx, controllertest.HostTeku)
	require.NoError(t, err)
	assert.Equal(t, []string{"teku"}, host.Status.Hold.Holders())

	ok("unhold", `{"hosts":["`+controllertest.HostTeku+`"]}`, teku)
	obeyHolds(t, st, controllertest.HostTeku)

	// Staff's hold: the twin does not lift it; the team's own hold is one
	// more next to it.
	ok("hold", `{"hosts":["`+controllertest.HostMixed+`"],"reason":"incident"}`, staff)
	obeyHolds(t, st, controllertest.HostMixed)

	code, msg = send("unhold", `{"hosts":["`+controllertest.HostMixed+`"],"holders":["staff"]}`, lighthouse)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "forbidden: upgradoor:team_manage does not lift the hold of staff on host lighthouse-geth-1", msg)

	ok("hold", `{"hosts":["`+controllertest.HostMixed+`"],"reason":"ours"}`, lighthouse)
	obeyHolds(t, st, controllertest.HostMixed)

	host, err = st.GetHost(ctx, controllertest.HostMixed)
	require.NoError(t, err)
	assert.Equal(t, []string{fleet.HolderStaff, "lighthouse"}, host.Status.Hold.Holders())
	assert.Equal(t, "incident", host.HoldOf(fleet.HolderStaff).Reason)

	rec := h.serve(userRoute{method: http.MethodGet, path: "/api/v1/hosts/" + controllertest.HostMixed}.request(lighthouse))
	detail := controllertest.DecodeJSON[views.HostDetail](t, rec)
	assert.Equal(t, host.Status.Hold.Holders(), views.Listing(&detail.HostSummary).Holders)

	res = ok("unhold", `{"hosts":["`+controllertest.HostMixed+`"]}`, lighthouse)
	assert.Equal(t, []string{controllertest.HostMixed}, res.Hosts)

	ok("unhold", `{"hosts":["`+controllertest.HostMixed+`"],"holders":["staff"]}`, staff)
	obeyHolds(t, st, controllertest.HostMixed)

	host, err = st.GetHost(ctx, controllertest.HostMixed)
	require.NoError(t, err)
	assert.Empty(t, host.Holds())

	// Through the twin a roll is what was asked for, "now" included.
	rollToken := provider.Grant(t, "+upgradoor:team_manage", "lighthouse")
	res = ok("roll", `{"hosts":["`+controllertest.HostMixed+`"],"containers":["lighthouse"],"staggered":true}`, rollToken)
	assert.Contains(t, res.Message, "staggered")
	assert.False(t, rollout(res.Rollout).Force)

	res = ok("roll", `{"hosts":["`+controllertest.HostMixed+`"],"containers":["lighthouse"]}`, rollToken)
	assert.Contains(t, res.Message, "now")
	assert.True(t, rollout(res.Rollout).Force)
}

// TestAuditNamesWhatPermittedIt: the audit names the one feature that
// allowed the request, whatever flags it carries. Admin includes every
// feature, which is then named plain.
func TestAuditNamesWhatPermittedIt(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	roll := func(extra string) string {
		return `{"hosts":["` + controllertest.HostMixed + `"],"containers":["lighthouse"]` + extra + `}`
	}
	lighthouse := []string{"lighthouse"}

	for _, tt := range []authzCase{
		{
			name: "roll now, twin", action: "roll", body: roll(""),
			services: "+upgradoor:team_manage", teams: lighthouse, by: "team_manage",
		},
		{
			name: "roll with force and ignoreBudget, twin", action: "roll", body: roll(`,"force":true,"ignoreBudget":true`),
			services: "+upgradoor:team_*", teams: lighthouse, by: "team_manage",
		},
		{
			name: "roll now, plain", action: "roll", body: roll(`,"force":true`),
			services: "+upgradoor:manage", teams: lighthouse, by: "manage",
		},
		{
			name: "roll, every twin and admin", action: "roll", body: roll(""),
			services: "+upgradoor:team_*:admin", teams: lighthouse, by: "manage",
		},
		{
			name: "roll, everything", action: "roll", body: roll(""),
			services: "+upgradoor:all", teams: lighthouse, by: "manage",
		},
		{
			name: "hold, twin and admin", action: "hold", body: `{"hosts":["` + controllertest.HostMixed + `"]}`,
			services: "+upgradoor:team_manage:admin", teams: lighthouse, by: "manage",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}
}

// TestSchedulerRefusalIsForbidden: what the scheduler refuses under its lock
// for the authorization of the request is answered like a refusal of the
// API: 403, the service name, the same text.
func TestSchedulerRefusalIsForbidden(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.AuthService = "upgradoor-dev" })
	controllertest.OwnershipFixture(h.store)

	h.sched.Set(func(f *controllertest.Scheduler) {
		f.ActionErr = fmt.Errorf("%w: team_manage does not lift the hold on host %s (set by staff)",
			scheduler.ErrForbidden, controllertest.HostMixed)
	})

	req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/x",
		strings.NewReader(`{"hosts":["`+controllertest.HostMixed+`"]}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.srv.actions()["unhold"](rec, req, &authz.User{
		Actor: controllertest.UserEmail, Services: "+upgradoor-dev:team_manage", Teams: []string{"geth"},
	})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, httpjson.ErrorBody{
		Error: "forbidden: upgradoor-dev:team_manage does not lift the hold on host lighthouse-geth-1 (set by staff)",
	}, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))
}

// TestEndLeaseLeavesATeamsHoldTheTeams goes through the real handlers, token
// verification, scheduler and store: ending a lease of the team's roll
// changes no hold. A team member who holds the team's host and ends its lease
// can lift that hold afterwards.
func TestEndLeaseLeavesATeamsHoldTheTeams(t *testing.T) {
	h, provider, st := realHarness(t)
	ctx := context.Background()

	syncOwned(t, st, controllertest.HostMixed, controllertest.Owned("geth", protocol.KindEL, "geth"), controllertest.Owned("lighthouse", protocol.KindCL, "lighthouse"))

	require.NoError(t, st.Commit(ctx, &store.Changes{Leases: []*fleet.Lease{{
		ID: "lease-1", Host: controllertest.HostMixed, State: fleet.LeaseRunning, GrantedBy: "scheduler",
		Needs: []protocol.Need{{
			ID: "image/lighthouse", Kind: protocol.NeedImage, Image: &protocol.ImageNeed{Container: "lighthouse"},
		}},
		Rollouts:  map[string]string{},
		GrantedAt: controllertest.Epoch, ExpiresAt: controllertest.Epoch.Add(time.Hour),
	}}}))

	send := func(action, body, authorization string) *httptest.ResponseRecorder {
		return h.serve(userRoute{
			method: http.MethodPost, path: "/api/v1/actions/" + action, body: body,
		}.request(authorization))
	}

	host := func() *fleet.Host {
		got, err := st.GetHost(ctx, controllertest.HostMixed)
		require.NoError(t, err)

		return got
	}

	team := provider.Grant(t, "-all,+upgradoor,+upgradoor:team_*", "lighthouse")
	hosts := `{"hosts":["` + controllertest.HostMixed + `"]}`

	rec := send("hold", `{"hosts":["`+controllertest.HostMixed+`"],"reason":"ours"}`, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	obeyHolds(t, st, controllertest.HostMixed)
	require.Equal(t, []string{"lighthouse"}, host().Status.Hold.Holders())

	rec = send("end-lease", `{"lease":"lease-1"}`, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, []string{"lighthouse"}, host().Status.Hold.Holders(), "ending a lease changes no hold")
	assert.Equal(t, "ours", host().Holds()[0].Reason)

	entries, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action.end-lease"})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.NotContains(t, entries[0].Detail, "hold")

	rec = h.serve(userRoute{method: http.MethodGet, path: "/api/v1/hosts/" + controllertest.HostMixed}.request(team))
	assert.Equal(t, "lighthouse", controllertest.DecodeJSON[views.HostDetail](t, rec).Hold.Holds[0].Holder)

	// The team lifts its hold, while the lease is still open.
	rec = send("unhold", hosts, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	obeyHolds(t, st, controllertest.HostMixed)
	assert.False(t, host().Held())

	// Lifting it again changes nothing and is not audited.
	before, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action."})
	require.NoError(t, err)

	rec = send("unhold", hosts, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, controllertest.DecodeJSON[scheduler.ActionResult](t, rec).Message, "1 host(s) had no such hold")

	after, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action."})
	require.NoError(t, err)
	assert.Len(t, after, len(before))
}

// TestAuthorizeEveryAction runs every action against every kind of login.
func TestAuthorizeEveryAction(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	samples := actionSamples(t)

	// Every registered action is in the samples, so a new action cannot be
	// added without a row here.
	sampled := make(map[string]struct{}, len(samples))
	for _, s := range samples {
		sampled[s.action] = struct{}{}
	}

	delete(sampled, actionSettings)

	// A host action is permitted per host: TestHostActionPermissions.
	registered := h.srv.actions()
	delete(registered, "host-action")
	assert.ElementsMatch(t, slices.Collect(maps.Keys(registered)),
		slices.Collect(maps.Keys(sampled)))

	owners := []string{"lighthouse"}
	strangers := []string{"besu", "reth"}

	for _, s := range samples {
		lacks := "forbidden: this login lacks the feature upgradoor:" + s.feature
		twin := authz.TeamFeaturePrefix + s.feature

		// What the team twin of the action's feature gets: the action, or
		// nothing when the feature has no twin.
		viaTeam := authzCase{refusal: lacks}
		if s.twin {
			viaTeam = authzCase{by: twin}
		}

		// admin includes every feature, which is then named.
		withAdmin := s.feature

		// What a grant of everything but this feature gets: admin, unless
		// the feature is admin.
		allBut := authzCase{by: withAdmin}
		if s.feature == authz.FeatureAdmin {
			allBut = authzCase{refusal: lacks}
		}

		notCovered := authzCase{refusal: lacks}
		if s.twin {
			notCovered = authzCase{refusalPrefix: "forbidden: upgradoor:" + twin + " does not cover "}
		}

		cases := []authzCase{
			{name: "open mode", open: true, by: authz.AuthorizedByOpen},
			{name: "open mode ignores a claim", open: true, services: "-all", by: authz.AuthorizedByOpen},
			{name: "no services claim", refusal: lacks},
			{name: "no services claim, in the owning team", teams: owners, refusal: lacks},
			{name: "basic only", services: "+upgradoor", teams: owners, refusal: lacks},
			{name: "basic spelled out", services: "+upgradoor:basic", teams: owners, refusal: lacks},
			{name: "only upgradoor, basic", services: "-all,+upgradoor", teams: owners, refusal: lacks},
			{name: "plain feature", services: "+upgradoor:" + s.feature, by: s.feature},
			{
				name: "plain feature, in no owning team", services: "+upgradoor:" + s.feature,
				teams: strangers, by: s.feature,
			},
			{
				name: "team twin, owning team", services: "+upgradoor:" + twin, teams: owners,
				by: viaTeam.by, refusal: viaTeam.refusal,
			},
			{
				name: "team twin, other teams", services: "+upgradoor:" + twin, teams: strangers,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{
				name: "team twin, no teams", services: "+upgradoor:" + twin,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{
				name: "plain and twin, other teams", teams: strangers,
				services: "+upgradoor:" + s.feature + ":" + twin, by: s.feature,
			},
			{name: "upgradoor:all", services: "+upgradoor:all", by: withAdmin},
			{name: "upgradoor:all, twin spelled too", services: "+upgradoor:all:" + twin, by: withAdmin},
			{name: "all:all", services: "+all:all", teams: strangers, by: withAdmin},
			{name: "only upgradoor:all", services: "-all,+upgradoor:all", by: withAdmin},
			{
				name: "upgradoor:team_*, owning team", services: "+upgradoor:team_*", teams: owners,
				by: viaTeam.by, refusal: viaTeam.refusal,
			},
			{
				name: "upgradoor:team_*, other teams", services: "+upgradoor:team_*", teams: strangers,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{name: "another service only", services: "+spamoor:all", teams: owners, refusal: lacks},
			{name: "no access to this service", services: "-all,+spamoor:all", teams: owners, refusal: lacks},
			{name: "service denied after the grant", services: "+upgradoor:all,-upgradoor", refusal: lacks},
			// "all" without the plain feature still holds admin, which
			// includes it.
			{
				name: "all but this feature", services: "+upgradoor:all:-" + s.feature,
				by: allBut.by, refusal: allBut.refusal,
			},
			{
				name: "all, this feature removed later", services: "+all:all,-upgradoor:" + s.feature,
				by: allBut.by, refusal: allBut.refusal,
			},
			{
				name:     "all but this feature and its twin",
				services: "+upgradoor:all:-" + s.feature + ":-" + twin, teams: owners,
				by: allBut.by, refusal: allBut.refusal,
			},
			{
				name: "admin includes every feature", services: "+upgradoor:admin", teams: owners,
				by: withAdmin,
			},
			{name: "a malformed grant", services: "+upgradoor:" + s.feature + ":", refusal: lacks},
		}

		for _, tt := range cases {
			tt.action, tt.body = s.action, s.body

			t.Run(s.action+" "+s.body+"/"+tt.name, func(t *testing.T) {
				runAuthz(t, h, &tt)
			})
		}
	}
}

// TestAuthorizeTargets: what the team twins cover.
func TestAuthorizeTargets(t *testing.T) {
	hosts := func(names ...string) string {
		raw, err := json.Marshal(names)
		require.NoError(t, err)

		return string(raw)
	}

	teamManage := "+upgradoor:team_manage"
	lighthouse, geth := []string{"lighthouse"}, []string{"geth"}
	both := []string{"geth", "lighthouse"}

	rollMixed := func(containers string) string {
		return `{"hosts":` + hosts(controllertest.HostMixed) + `,"containers":` + containers + `}`
	}

	tests := []authzCase{
		// A roll with named containers: every one of them.
		{
			name: "roll, own container", action: "roll", body: rollMixed(`["lighthouse"]`),
			services: teamManage, teams: lighthouse, by: "team_manage",
		},
		{
			name: "roll, own containers", action: "roll",
			body:     rollMixed(`["lighthouse","lighthouse-validator"]`),
			services: teamManage, teams: lighthouse, by: "team_manage",
		},
		{
			name: "roll, mixed ownership", action: "roll", body: rollMixed(`["lighthouse","geth"]`),
			services: teamManage, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll, mixed ownership, in both teams", action: "roll",
			body:     rollMixed(`["lighthouse","geth"]`),
			services: teamManage, teams: both, by: "team_manage",
		},
		{
			name: "roll, the other team's container", action: "roll", body: rollMixed(`["geth"]`),
			services: teamManage, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll, a container of its team", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostTeku) + `,"containers":["teku"]}`,
			services: teamManage, teams: []string{"teku"}, by: "team_manage",
		},
		{
			name: "roll, a container of another team", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostTeku) + `,"containers":["teku"]}`,
			services: teamManage, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover container teku " +
				"on host teku-geth-1 (owners: teku)",
		},
		{
			name: "roll, one container on two hosts", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostTeku) + `,"containers":["geth"]}`,
			services: teamManage, teams: geth, by: "team_manage",
		},
		{
			name: "roll, a container one host does not have", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostUnowned) + `,"containers":["geth"]}`,
			services: teamManage, teams: geth,
			refusal: "forbidden: upgradoor:team_manage does not cover container geth " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "roll, unknown container", action: "roll", body: rollMixed(`["nethermind"]`),
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover container nethermind " +
				"on host lighthouse-geth-1 (owners: none)",
		},
		{
			name: "roll, unmanaged container", action: "roll", body: rollMixed(`["debug"]`),
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover container debug " +
				"on host lighthouse-geth-1 (owners: none)",
		},
		{
			name: "roll, unknown container, plain feature", action: "roll",
			body: rollMixed(`["nethermind"]`), services: "+upgradoor:manage", by: "manage",
		},

		// A roll without containers: every managed container of the host.
		{
			name: "roll a host, one team of two", action: "roll", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamManage, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll a host, both teams", action: "roll", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamManage, teams: both, by: "team_manage",
		},
		{
			name: "roll hosts, second not covered", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostTeku) + `}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover container teku " +
				"on host teku-geth-1 (owners: teku)",
		},
		{
			name: "roll a host nobody owns", action: "roll", body: `{"hosts":` + hosts(controllertest.HostUnowned) + `}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover container bootnode " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "roll a host that never synced", action: "roll", body: `{"hosts":` + hosts(controllertest.HostFresh) + `}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover host fresh-1 (owners: none)",
		},
		{
			name: "roll an unknown host", action: "roll", body: `{"hosts":` + hosts(controllertest.HostGhost) + `}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover host ghost-1 (owners: none)",
		},
		{
			name: "roll an unknown host, named container", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostGhost) + `,"containers":["geth"]}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover container geth " +
				"on host ghost-1 (owners: none)",
		},
		{
			// The scheduler answers the unknown host.
			name: "roll an unknown host, plain feature", action: "roll",
			body: `{"hosts":` + hosts(controllertest.HostGhost) + `}`, services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "roll, no target", action: "roll", body: `{}`, services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage needs a target that one of your teams owns, " +
				"and the request names none",
		},

		// Host-level actions: one owned container is enough.
		{
			name: "hold, the CL team", action: "hold", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamManage, teams: lighthouse, by: "team_manage",
		},
		{
			name: "hold, the EL team", action: "hold", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamManage, teams: geth, by: "team_manage",
		},
		{
			name: "hold, a team without a container there", action: "hold",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamManage, teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_manage does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "hold two hosts, the EL team", action: "hold",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostTeku) + `}`,
			services: teamManage, teams: geth, by: "team_manage",
		},
		{
			name: "hold two hosts, second not covered", action: "hold",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostTeku) + `}`,
			services: teamManage, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover host teku-geth-1 " +
				"(owners: geth, teku)",
		},
		{
			name: "hold a host nobody owns", action: "hold", body: `{"hosts":` + hosts(controllertest.HostUnowned) + `}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "hold a host that never synced", action: "hold",
			body:     `{"hosts":` + hosts(controllertest.HostFresh) + `}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover host fresh-1 (owners: none)",
		},
		{
			name: "hold an unknown host", action: "hold", body: `{"hosts":` + hosts(controllertest.HostGhost) + `}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover host ghost-1 (owners: none)",
		},
		{
			name: "reboot an unknown host, plain feature", action: "reboot",
			body: `{"hosts":` + hosts(controllertest.HostGhost) + `}`, services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "hold, no target", action: "hold", body: `{"hosts":[]}`,
			services: "+upgradoor:team_manage", teams: both,
			refusal: "forbidden: upgradoor:team_manage needs a target that one of your teams owns, " +
				"and the request names none",
		},
		{
			name: "unhold, a host nobody owns", action: "unhold", body: `{"hosts":` + hosts(controllertest.HostUnowned) + `}`,
			services: "+upgradoor:team_manage", teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "the twin of another feature", action: "hold", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: "+upgradoor:team_manage_image:team_details", teams: both,
			refusal: "forbidden: this login lacks the feature upgradoor:manage",
		},
		{
			name: "reboot, every twin", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: "+upgradoor:team_*", teams: geth,
			refusal: "forbidden: this login lacks the feature upgradoor:manage",
		},

		// The flags and the selector all need nothing more; the twin applies
		// its rule to every host.
		{
			name: "reboot with force, plain", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `,"force":true,"ignoreBudget":true}`,
			services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "reboot all, plain", action: "reboot", body: `{"all":true}`, services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "roll with force and ignoreBudget, team", action: "roll",
			body:     rollMixed(`["lighthouse"]`)[:len(rollMixed(`["lighthouse"]`))-1] + `,"force":true,"ignoreBudget":true}`,
			services: teamManage, teams: lighthouse, by: "team_manage",
		},
		{
			name: "roll with force, team, another container", action: "roll",
			body:     rollMixed(`["geth"]`)[:len(rollMixed(`["geth"]`))-1] + `,"force":true}`,
			services: teamManage, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll all, plain", action: "roll", body: `{"all":true}`, services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "roll all, team: every host", action: "roll", body: `{"all":true,"containers":["lighthouse"]}`,
			services: teamManage, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover container lighthouse " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "roll all, team and admin: admin includes manage", action: "roll",
			body: `{"all":true}`, services: "+upgradoor:team_manage:admin", teams: both, by: "manage",
		},

		// One rollout, or everything.
		{
			name: "pause one rollout with admin only", action: "pause", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:admin", by: "manage",
		},
		{
			name: "pause everything with manage only", action: "pause", body: `{}`,
			services: "+upgradoor:manage",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin",
		},
		{
			name: "resume everything with manage only", action: "resume", body: `{}`,
			services: "+upgradoor:manage",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin",
		},
		{
			name: "abort a rollout for no team with a team twin", action: "abort", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:team_*", teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover rollout r-1: it is for no team",
		},

		// Retry: hosts are a recovery, none is the whole rollout.
		{
			name: "retry hosts, team", action: "retry",
			body:     `{"rollout":"r-1","hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamManage, teams: lighthouse, by: "team_manage",
		},
		{
			name: "retry hosts, team, not covered", action: "retry",
			body:     `{"rollout":"r-1","hosts":` + hosts(controllertest.HostMixed, controllertest.HostUnowned) + `}`,
			services: teamManage, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover rollout r-1 on host bootnode-1: " +
				"it rolls no container there",
		},
		{
			name: "retry hosts of a reboot rollout, team", action: "retry",
			body:     `{"rollout":"r-reboot","hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover rollout r-reboot: it rolls no containers (reboot)",
		},
		{
			name: "retry a rollout for no team, team", action: "retry", body: `{"rollout":"r-1"}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover rollout r-1: it is for no team",
		},
		{
			name: "retry the rollout, plain", action: "retry", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "retry hosts, plain", action: "retry",
			body:     `{"rollout":"r-1","hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "skip, team, not covered", action: "skip",
			body:     `{"rollout":"r-1","hosts":` + hosts(controllertest.HostTeku) + `}`,
			services: teamManage, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_manage does not cover container teku on host teku-geth-1 " +
				"(owners: teku)",
		},

		// A lease is the team's when it only rolls the team's containers.
		{
			name: "end a lease of the team's roll", action: "end-lease", body: `{"lease":"` + controllertest.LeaseMixed + `"}`,
			services: teamManage, teams: lighthouse, by: "team_manage",
		},
		{
			name: "end a lease that reboots as well", action: "end-lease",
			body: `{"lease":"` + controllertest.LeaseReboot + `"}`, services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover lease lease-reboot: it carries a reboot",
		},
		{
			name: "end a lease that reboots as well, plain feature", action: "end-lease",
			body: `{"lease":"` + controllertest.LeaseReboot + `"}`, services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "end a closed lease of an owned host", action: "end-lease",
			body: `{"lease":"` + controllertest.LeaseDone + `"}`, services: teamManage, teams: geth, by: "team_manage",
		},
		{
			name: "end a lease of another team's roll", action: "end-lease",
			body:     `{"lease":"` + controllertest.LeaseMixed + `"}`,
			services: teamManage, teams: geth,
			refusal: "forbidden: upgradoor:team_manage does not cover container lighthouse on host lighthouse-geth-1 " +
				"(owners: lighthouse)",
		},
		{
			name: "end a lease of a host nobody owns", action: "end-lease",
			body:     `{"lease":"` + controllertest.LeaseUnowned + `"}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover container bootnode on host bootnode-1 (owners: none)",
		},
		{
			name: "end an unknown lease", action: "end-lease", body: `{"lease":"` + controllertest.LeaseGhost + `"}`,
			services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage does not cover lease lease-ghost (owners: none)",
		},
		{
			name: "end an unknown lease, plain feature", action: "end-lease",
			body: `{"lease":"` + controllertest.LeaseGhost + `"}`, services: "+upgradoor:manage", by: "manage",
		},
		{
			name: "end no lease", action: "end-lease", body: `{}`, services: teamManage, teams: both,
			refusal: "forbidden: upgradoor:team_manage needs a target that one of your teams owns, " +
				"and the request names none",
		},
	}

	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}
}

// TestAuthorizeConfiguredService: the features are features of the service
// the controller was told it is.
func TestAuthorizeConfiguredService(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.AuthService = "upgradoor-dev" })
	controllertest.OwnershipFixture(h.store)

	body := `{"hosts":["` + controllertest.HostMixed + `"]}`

	for _, tt := range []authzCase{
		{
			name: "the default name grants nothing", services: "+upgradoor:all",
			refusal: "forbidden: this login lacks the feature upgradoor-dev:manage",
		},
		{name: "the configured name", services: "+upgradoor-dev:manage", by: "manage"},
		{name: "all services", services: "+all:all", by: "manage"},
		{
			name: "the team twin", services: "+upgradoor-dev:team_manage", teams: []string{"geth"},
			by: "team_manage",
		},
		{
			name: "the team twin, not covered", services: "+upgradoor-dev:team_manage",
			teams: []string{"teku"},
			refusal: "forbidden: upgradoor-dev:team_manage does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
	} {
		tt.action, tt.body = "hold", body

		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}

	assert.Equal(t, "upgradoor-dev", controllertest.DecodeJSON[views.Config](t, h.get("/api/v1/config")).AuthService)
}

// TestAuthorizeStoreError: when ownership cannot be read, the request fails;
// it is not allowed and it is not blamed on the login.
func TestAuthorizeStoreError(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)
	h.store.SetErr(assert.AnError)

	for _, tt := range []struct{ action, body string }{
		{"hold", `{"hosts":["` + controllertest.HostMixed + `"]}`},
		{"end-lease", `{"lease":"` + controllertest.LeaseMixed + `"}`},
	} {
		req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/x", strings.NewReader(tt.body))
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		h.srv.actions()[tt.action](rec, req, &authz.User{
			Actor: controllertest.UserEmail, Services: "+upgradoor:team_*", Teams: []string{"geth"},
		})

		require.Equal(t, http.StatusInternalServerError, rec.Code, tt.action)
		assert.Equal(t, httpjson.ErrorBody{Error: "internal error"}, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))
	}

	assert.Empty(t, h.sched.Recorded())
}

// TestPermissionMatrix runs every registered action for each role against
// targets of the role's team, of another team, of no team, and with work
// riding along, and checks allow or deny by the permission model of authz.
// A client dev holds every team twin and acts on the team's own containers
// only, flags included; staff holds details, manage, manage_image and
// manage_flags; admin includes every feature. An action registered without a
// row fails it.
func TestPermissionMatrix(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	roll := func(container string) protocol.Need {
		return protocol.Need{
			ID: "image/" + container, Kind: protocol.NeedImage, Image: &protocol.ImageNeed{Container: container},
		}
	}

	h.store.Lock()
	h.store.Rollouts = append(h.store.Rollouts,
		&fleet.Rollout{ID: "r-geth", Kind: fleet.RolloutGroup, ContainerKind: protocol.KindEL, Team: "geth",
			State: fleet.RolloutActive, Hosts: []fleet.RolloutHost{{Host: controllertest.HostMixed, State: fleet.HostPending}}},
		&fleet.Rollout{ID: "r-ride", Kind: fleet.RolloutGroup, ContainerKind: protocol.KindCL, Team: "lighthouse",
			State: fleet.RolloutActive, Hosts: []fleet.RolloutHost{
				{Host: controllertest.HostMixed, State: fleet.HostFailed, LeaseID: "lease-ride"},
			}},
	)
	h.store.Leases = append(h.store.Leases, &fleet.Lease{
		ID: "lease-ride", Host: controllertest.HostMixed, State: fleet.LeaseRunning,
		Needs: []protocol.Need{roll("lighthouse"), {ID: protocol.RebootNeedID, Kind: protocol.NeedReboot}},
	})
	h.store.Rollouts[2].Hosts = []fleet.RolloutHost{{Host: controllertest.HostMixed, State: fleet.HostFailed}}
	h.store.Unlock()

	const (
		own      = "own"
		other    = "other team's"
		teamless = "teamless"
		ride     = "ride-along"
	)

	mixed, teku, unowned := controllertest.HostMixed, controllertest.HostTeku, controllertest.HostUnowned
	hosts := func(names ...string) string { return `{"hosts":["` + strings.Join(names, `","`) + `"]}` }
	rollOf := func(host, container, extra string) string {
		return fmt.Sprintf(`{"hosts":[%q],"containers":[%q],"staggered":true%s}`, host, container, extra)
	}
	override := func(host, container string) string {
		return fmt.Sprintf(`{"host":%q,"container":%q,"image":"x/y:z"}`, host, container)
	}
	flags := func(host, container string) string {
		return fmt.Sprintf(`{"host":%q,"container":%q,"addFlags":["--debug"]}`, host, container)
	}
	clientOverride := func(client, kind, host string) string {
		return fmt.Sprintf(`{"client":%q,"kind":%q,"hosts":[%q],"part":"image","image":"x/y:z"}`, client, kind, host)
	}
	rollout := func(id string) string { return `{"rollout":"` + id + `"}` }
	member := func(id, host string) string { return fmt.Sprintf(`{"rollout":%q,"hosts":[%q]}`, id, host) }
	lease := func(id string) string { return `{"lease":"` + id + `"}` }
	approve := func(id string) string { return fmt.Sprintf(`{"rollout":%q,"host":%q}`, id, mixed) }

	// team says whether the team member is allowed; staff is allowed unless
	// the row is admin's only.
	type row struct {
		action, target, body string
		team, adminOnly      bool
	}

	rows := []row{
		{"roll", own, rollOf(mixed, "lighthouse", ""), true, false},
		{"roll", other, rollOf(mixed, "geth", ""), false, false},
		{"roll", teamless, rollOf(unowned, "bootnode", ""), false, false},
		{"roll", "own, force", rollOf(mixed, "lighthouse", `,"force":true,"ignoreBudget":true`), true, false},
		{"roll", "own, now", fmt.Sprintf(`{"hosts":[%q],"containers":["lighthouse"]}`, mixed), true, false},
		{"roll", "other team's, force", rollOf(mixed, "geth", `,"force":true`), false, false},
		{"roll", "all", `{"all":true,"staggered":true}`, false, false},
		{"roll", "all, own container", `{"all":true,"containers":["lighthouse"]}`, false, false},
		{"reboot", own, hosts(mixed), false, false},
		{"reboot", "all", `{"all":true,"force":true}`, false, false},
		{"hold", own, hosts(mixed), true, false},
		{"hold", other, hosts(teku), false, false},
		{"hold", teamless, hosts(unowned), false, false},
		{"unhold", own, hosts(mixed), true, false},
		{"unhold", "staff's", `{"hosts":["` + mixed + `"],"holders":["staff"]}`, false, false},
		{"unhold", other, `{"hosts":["` + teku + `"],"holders":["teku"]}`, false, false},
		{"mark-offline", own, `{"hosts":["` + mixed + `"],"reason":"x"}`, false, false},
		{"unmark-offline", own, hosts(mixed), false, false},
		{"image-override", own, override(mixed, "lighthouse"), true, false},
		{"image-override", other, override(mixed, "geth"), false, false},
		{"image-override", teamless, override(unowned, "bootnode"), false, false},
		{"flag-override", own, flags(mixed, "lighthouse"), true, false},
		{"flag-override", other, flags(mixed, "geth"), false, false},
		{"client-override", own, clientOverride("lighthouse", "cl", mixed), true, false},
		{"client-override", other, clientOverride("teku", "cl", teku), false, false},
		{
			"client-override", "own flags",
			fmt.Sprintf(`{"client":"lighthouse","kind":"cl","hosts":[%q],"part":"flags","addFlags":["--debug"]}`, mixed),
			true, false,
		},
		{"client-policy", own, `{"client":"lighthouse","policy":"manual"}`, true, false},
		{"client-policy", other, `{"client":"geth","policy":"manual"}`, false, false},
		{"forget", own, hosts(mixed), false, true},
		{"end-lease", own, lease(controllertest.LeaseMixed), true, false},
		{"end-lease", other, lease(controllertest.LeaseDone), false, false},
		{"end-lease", teamless, lease(controllertest.LeaseUnowned), false, false},
		{"end-lease", ride, lease("lease-ride"), false, false},
		{"pause", own, rollout("r-lighthouse"), true, false},
		{"pause", other, rollout("r-geth"), false, false},
		{"pause", teamless, rollout("r-1"), false, false},
		{"pause", "everything", `{}`, false, true},
		{"resume", own, rollout("r-lighthouse"), true, false},
		{"resume", other, rollout("r-geth"), false, false},
		{"resume", "everything", `{}`, false, true},
		{"abort", own, rollout("r-lighthouse"), true, false},
		{"abort", other, rollout("r-geth"), false, false},
		{"abort", teamless, rollout("r-reboot"), false, false},
		{"ignore-gate", own, rollout("r-lighthouse"), true, false},
		{"ignore-gate", other, rollout("r-geth"), false, false},
		{"approve-lease", own, approve("r-lighthouse"), true, false},
		{"approve-lease", other, approve("r-geth"), false, false},
		{"retry", own, rollout("r-lighthouse"), true, false},
		{"retry", other, rollout("r-geth"), false, false},
		{"retry", "own member", member("r-lighthouse", mixed), true, false},
		{"retry", "other team's member on a shared host", member("r-geth", mixed), false, false},
		{"retry", "own member of a manual roll across teams", member("r-1", mixed), true, false},
		{"retry", "other team's member of a manual roll", member("r-1", teku), false, false},
		{"retry", ride, member("r-ride", mixed), false, false},
		{"skip", own, member("r-lighthouse", mixed), true, false},
		{"skip", "other team's member on a shared host", member("r-geth", mixed), false, false},
		{"skip", teamless, member("r-reboot", mixed), false, false},
		{"skip", ride, member("r-ride", mixed), false, false},
	}

	// A host action is permitted by its own configuration:
	// TestHostActionPermissions.
	covered := map[string]bool{"host-action": true}
	for _, r := range rows {
		covered[r.action] = true
	}

	for name := range h.srv.actions() {
		assert.True(t, covered[name], "the action %s has no row in the permission matrix", name)
	}

	lighthouse := []string{"lighthouse"}
	staff := "+upgradoor:details:manage:manage_image:manage_flags"
	roles := []struct {
		name string
		user *authz.User
		may  func(r row) bool
	}{
		{"observer", approver("+upgradoor", lighthouse...), func(row) bool { return false }},
		{"client dev", approver("+upgradoor:team_*", lighthouse...), func(r row) bool { return r.team }},
		{"staff", approver(staff, lighthouse...), func(r row) bool { return !r.adminOnly }},
		{"admin", approver("+upgradoor:admin", lighthouse...), func(row) bool { return true }},
	}

	for _, role := range roles {
		for _, r := range rows {
			t.Run(role.name+"/"+r.action+"/"+r.target, func(t *testing.T) {
				rec := postAction(h, role.user, r.action, r.body)

				if role.may(r) {
					assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				} else {
					assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				}
			})
		}
	}
}
