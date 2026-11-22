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

	assert.False(t, held(controllertest.HostMixed))
	assert.False(t, held(controllertest.HostUnowned))
	assert.Empty(t, audit(), "a refusal is not audited")

	// Allowed through the team twin.
	rec := hold("hold", controllertest.HostMixed, lighthouse)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, held(controllertest.HostMixed))

	entries := audit()
	require.Len(t, entries, 1)
	assert.Equal(t, "action.hold", entries[0].Action)
	assert.Equal(t, controllertest.UserEmail, entries[0].Actor)
	assert.Equal(t, controllertest.HostMixed, entries[0].Target)
	assert.Equal(t, "team_hold", entries[0].Detail["authorizedBy"])
	assert.Equal(t, []any{"lighthouse"}, entries[0].Detail["teams"])
	assert.Equal(t, "debugging", entries[0].Detail["reason"])
	assert.Contains(t, entries[0].Detail, "before")
	assert.Contains(t, entries[0].Detail, "after")

	// The other team cannot take the hold back.
	rec = hold("unhold", controllertest.HostMixed, teku)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.True(t, held(controllertest.HostMixed))
	assert.Len(t, audit(), 1)

	// A staff login can, and may name any host.
	staff := provider.Grant(t, "+upgradoor:all", "panda")

	rec = hold("unhold", controllertest.HostMixed, staff)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, held(controllertest.HostMixed))

	entries = audit()
	require.Len(t, entries, 2)
	assert.Equal(t, "action.unhold", entries[0].Action)
	assert.Equal(t, "hold", entries[0].Detail["authorizedBy"])
	assert.Equal(t, []any{"panda"}, entries[0].Detail["teams"])

	// The global pause needs admin; with rollouts alone it stays as it was.
	rec = send(http.MethodPost, "/api/v1/actions/pause", `{}`, provider.Grant(t, "+upgradoor:supervise"))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	settings := controllertest.DecodeJSON[views.SettingsBody](t, send(http.MethodGet, "/api/v1/settings", "", reader))
	assert.False(t, settings.Settings.Paused)

	rec = send(http.MethodPost, "/api/v1/actions/pause", `{"reason":"incident"}`,
		provider.Grant(t, "+upgradoor:admin"))
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

	body := string(controllertest.MustJSON(t, changed))

	for _, authorization := range []string{reader, lighthouse, provider.Grant(t, "+upgradoor:supervise:hold")} {
		rec = send(http.MethodPut, "/api/v1/settings", body, authorization)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, httpjson.ErrorBody{Error: "forbidden: this login lacks the feature upgradoor:admin"},
			controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))
	}

	// Refused before the body is looked at.
	rec = send(http.MethodPut, "/api/v1/settings", `{"minPeers":`, reader)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	got := controllertest.DecodeJSON[views.SettingsBody](t, send(http.MethodGet, "/api/v1/settings", "", reader))
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

	got = controllertest.DecodeJSON[views.SettingsBody](t, send(http.MethodGet, "/api/v1/settings", "", reader))
	assert.Equal(t, 7, got.Settings.MinPeers)

	// Reads are not audited.
	all, err := st.ListAudit(ctx, store.AuditFilter{})
	require.NoError(t, err)

	for _, e := range all {
		assert.NotContains(t, e.Action, "read")
	}
}

// TestRolloutActionRequirements: a retry with hosts recovers hosts, whatever
// the rollout's state (an aborted one is refused by the scheduler); pause and
// resume (approve) of a rollout are the team's of its client; abort needs
// supervise itself.
func TestRolloutActionRequirements(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	retry := fmt.Sprintf(`{"rollout":"r-aborted","hosts":[%q]}`, controllertest.HostMixed)
	lighthouse := []string{"lighthouse"}

	for _, tt := range []authzCase{
		{
			name: "retry hosts of an aborted rollout, the team twin", action: "retry", body: retry,
			services: "+upgradoor:team_recover", teams: lighthouse, by: "team_recover",
		},
		{
			name: "retry an aborted rollout, recover", action: "retry", body: `{"rollout":"r-aborted"}`,
			services: "+upgradoor:recover",
			refusal:  "forbidden: this login lacks the feature upgradoor:supervise",
		},
		{
			name: "approve the team's client", action: "resume", body: `{"rollout":"r-lighthouse"}`,
			services: "+upgradoor:team_supervise", teams: lighthouse, by: "team_supervise",
		},
		{
			name: "approve another team's client", action: "resume", body: `{"rollout":"r-lighthouse"}`,
			services: "+upgradoor:team_supervise", teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_supervise does not cover client lighthouse: container " +
				"lighthouse on host lighthouse-geth-1 (owners: lighthouse)",
		},
		{
			name: "pause a rollout for no team", action: "pause", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:team_supervise", teams: lighthouse,
			refusal: "forbidden: upgradoor:team_supervise does not cover rollout r-1: it is for no team",
		},
		{
			name: "pause an unknown rollout", action: "pause", body: `{"rollout":"r-ghost"}`,
			services: "+upgradoor:team_supervise", teams: lighthouse,
			refusal: "forbidden: upgradoor:team_supervise does not cover rollout r-ghost (owners: none)",
		},
		{
			name: "abort the team's client", action: "abort", body: `{"rollout":"r-lighthouse"}`,
			services: "+upgradoor:team_*", teams: lighthouse,
			refusal: "forbidden: this login lacks the feature upgradoor:supervise",
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
		Actor: controllertest.UserEmail, Services: "+upgradoor:team_supervise", Teams: lighthouse,
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

// TestTeamTwinDoesNotTouchStaffHolds: team_hold lifts a hold that one of
// its teams set through team_hold. A hold that staff, the scheduler, the end
// of a lease or another team set is not the twin's to lift, nor to set again
// as its own.
func TestTeamTwinDoesNotTouchStaffHolds(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	h.store.Lock()
	// Staff's: HoldBy is empty.
	h.store.Hosts[controllertest.HostMixed].HoldWanted = true
	h.store.Hosts[controllertest.HostMixed].HoldReason = "lease ended by operator"
	// A team's.
	h.store.Hosts[controllertest.HostTeku].HoldWanted = true
	h.store.Hosts[controllertest.HostTeku].HoldBy = fleet.TeamHold([]string{"nimbus", "teku"})
	h.store.Unlock()

	hosts := func(names ...string) string { return `{"hosts":["` + strings.Join(names, `","`) + `"]}` }
	twin := "+upgradoor:team_hold"

	for _, tt := range []authzCase{
		{
			name: "unhold a staff hold", action: "unhold", body: hosts(controllertest.HostMixed),
			services: twin, teams: []string{"lighthouse"},
			refusal: "forbidden: upgradoor:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)",
		},
		{
			name: "unhold a staff hold, every twin", action: "unhold", body: hosts(controllertest.HostMixed),
			services: "+upgradoor:team_*", teams: []string{"geth", "lighthouse"},
			refusal: "forbidden: upgradoor:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)",
		},
		{
			name: "hold again what staff holds", action: "hold", body: hosts(controllertest.HostMixed),
			services: twin, teams: []string{"lighthouse"},
			refusal: "forbidden: upgradoor:team_hold does not change the hold on host lighthouse-geth-1 (set by staff)",
		},
		{
			name: "unhold a team's hold, the team", action: "unhold", body: hosts(controllertest.HostTeku),
			services: twin, teams: []string{"teku"}, by: "team_hold",
		},
		{
			name: "unhold a team's hold, another owner of the host", action: "unhold", body: hosts(controllertest.HostTeku),
			services: twin, teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_hold does not lift the hold on host teku-geth-1 (set by team nimbus, teku)",
		},
		{
			name: "unhold a team's hold, a team without a container there", action: "unhold",
			body: hosts(controllertest.HostTeku), services: twin, teams: []string{"lighthouse"},
			refusal: "forbidden: upgradoor:team_hold does not cover host teku-geth-1 (owners: geth, teku)",
		},
		{
			name: "hold again what the team holds", action: "hold", body: hosts(controllertest.HostTeku),
			services: twin, teams: []string{"teku"}, by: "team_hold",
		},
		{
			name: "hold again what another team holds", action: "hold", body: hosts(controllertest.HostTeku),
			services: twin, teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_hold does not change the hold on host teku-geth-1 (set by team nimbus, teku)",
		},
		{
			name: "one of two is staff's", action: "unhold", body: hosts(controllertest.HostTeku, controllertest.HostMixed),
			services: twin, teams: []string{"geth", "teku"},
			refusal: "forbidden: upgradoor:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)",
		},
		{
			name: "the plain feature lifts any hold", action: "unhold", body: hosts(controllertest.HostTeku, controllertest.HostMixed),
			services: "+upgradoor:hold", by: "hold",
		},
		{
			name: "the plain feature holds over any hold", action: "hold", body: hosts(controllertest.HostMixed),
			services: "+upgradoor:hold", teams: []string{"lighthouse"}, by: "hold",
		},
		{
			name: "a host that is not held", action: "unhold", body: hosts(controllertest.HostUnowned),
			services: twin, teams: []string{"geth"},
			refusal: "forbidden: upgradoor:team_hold does not cover host bootnode-1 (owners: none)",
		},
		{
			// Other host-level actions do not look at the hold.
			name: "reboot a host staff holds", action: "reboot", body: hosts(controllertest.HostMixed),
			services: "+upgradoor:team_reboot", teams: []string{"geth"}, by: "team_reboot",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runAuthz(t, h, &tt)
		})
	}

	// The host views say whose hold it is.
	list := controllertest.DecodeJSON[views.Hosts](t, h.get("/api/v1/hosts"))
	by := make(map[string]string, len(list.Hosts))

	for _, host := range list.Hosts {
		by[host.Name] = host.HoldBy
	}

	assert.Equal(t, map[string]string{
		controllertest.HostMixed: "staff", controllertest.HostTeku: "team:nimbus,teku", controllertest.HostUnowned: "", controllertest.HostFresh: "",
	}, by)
	assert.Equal(t, "staff", controllertest.DecodeJSON[views.HostDetail](t, h.get("/api/v1/hosts/"+controllertest.HostMixed)).HoldBy)
	assert.NotContains(t, h.get("/api/v1/public/hosts").Body.String(), "holdBy")
}

// TestTeamPathIsStaggered: a roll or reboot "now" skips the order and the
// wait for readiness, like force does. Through the team twin alone the
// request reaches the scheduler staggered; with the plain feature, or with
// admin next to the twin, it is what was asked for.
func TestTeamPathIsStaggered(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	roll := `{"hosts":["` + controllertest.HostMixed + `"],"containers":["lighthouse"]}`
	reboot := `{"hosts":["` + controllertest.HostMixed + `"]}`
	lighthouse := []string{"lighthouse"}

	for _, tt := range []struct {
		name      string
		action    string
		body      string
		services  string
		staggered bool
	}{
		{"roll now, team twin", "roll", roll, "+upgradoor:team_roll", true},
		{"roll now, every twin", "roll", roll, "+upgradoor:team_*", true},
		{"roll now, plain feature", "roll", roll, "+upgradoor:roll", false},
		{"roll now, plain and twin", "roll", roll, "+upgradoor:roll:team_roll", false},
		{"roll now, twin and admin", "roll", roll, "+upgradoor:team_roll:admin", false},
		{"roll now, everything", "roll", roll, "+upgradoor:all", false},
		{"reboot now, team twin", "reboot", reboot, "+upgradoor:team_reboot", true},
		{"reboot now, plain feature", "reboot", reboot, "+upgradoor:reboot", false},
		{"reboot now, twin and admin", "reboot", reboot, "+upgradoor:team_reboot:admin", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(h.sched.Recorded())

			req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/x", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")

			rec := httptest.NewRecorder()
			h.srv.actions()[tt.action](rec, req, &authz.User{
				Actor: controllertest.UserEmail, Services: tt.services, Teams: lighthouse,
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			calls := h.sched.Recorded()
			require.Len(t, calls, before+1)

			switch got := calls[before].Req.(type) {
			case scheduler.RollRequest:
				assert.Equal(t, tt.staggered, got.Staggered)
				assert.False(t, got.Force)
				assert.False(t, got.IgnoreBudget)
			case scheduler.RebootRequest:
				assert.Equal(t, tt.staggered, got.Staggered)
				assert.False(t, got.Force)
			default:
				require.Fail(t, "unexpected request", "%T", got)
			}
		})
	}

	// Open mode asks for nothing of the kind.
	rec := h.sendJSON(http.MethodPost, "/api/v1/actions/roll", roll)
	require.Equal(t, http.StatusOK, rec.Code)

	calls := h.sched.Recorded()
	got, ok := calls[len(calls)-1].Req.(scheduler.RollRequest)
	require.True(t, ok)
	assert.False(t, got.Staggered)
}

// TestOwnershipIsExact: no teams own nothing, also where nobody owns
// anything; and a team is its name, byte for byte.
func TestOwnershipIsExact(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	hosts := func(name string) string { return `{"hosts":["` + name + `"]}` }

	for _, tt := range []authzCase{
		{
			name: "no teams, a host nobody owns", action: "reboot", body: hosts(controllertest.HostUnowned),
			services: "+upgradoor:team_reboot",
			refusal:  "forbidden: upgradoor:team_reboot does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "an empty team list, a host nobody owns", action: "hold", body: hosts(controllertest.HostUnowned),
			services: "+upgradoor:team_hold", teams: []string{},
			refusal: "forbidden: upgradoor:team_hold does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "no teams, roll a host nobody owns", action: "roll", body: hosts(controllertest.HostUnowned),
			services: "+upgradoor:team_roll",
			refusal: "forbidden: upgradoor:team_roll does not cover container bootnode " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "no teams, roll a container nobody owns", action: "roll",
			body:     `{"hosts":["` + controllertest.HostUnowned + `"],"containers":["bootnode"]}`,
			services: "+upgradoor:team_roll",
			refusal: "forbidden: upgradoor:team_roll does not cover container bootnode " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "no teams, a host that never synced", action: "reboot", body: hosts(controllertest.HostFresh),
			services: "+upgradoor:team_reboot",
			refusal:  "forbidden: upgradoor:team_reboot does not cover host fresh-1 (owners: none)",
		},
		{
			name: "an empty team name", action: "reboot", body: hosts(controllertest.HostUnowned),
			services: "+upgradoor:team_reboot", teams: []string{""},
			refusal: "forbidden: upgradoor:team_reboot does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "another case", action: "reboot", body: hosts(controllertest.HostMixed),
			services: "+upgradoor:team_reboot", teams: []string{"Lighthouse", "GETH"},
			refusal: "forbidden: upgradoor:team_reboot does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "another case, a roll", action: "roll",
			body:     `{"hosts":["` + controllertest.HostMixed + `"],"containers":["lighthouse"]}`,
			services: "+upgradoor:team_roll", teams: []string{"Lighthouse"},
			refusal: "forbidden: upgradoor:team_roll does not cover container lighthouse " +
				"on host lighthouse-geth-1 (owners: lighthouse)",
		},
		{
			name: "a prefix of the team", action: "reboot", body: hosts(controllertest.HostMixed),
			services: "+upgradoor:team_reboot", teams: []string{"light", "lighthouse2", " lighthouse"},
			refusal: "forbidden: upgradoor:team_reboot does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "the name", action: "reboot", body: hosts(controllertest.HostMixed),
			services: "+upgradoor:team_reboot", teams: []string{"Lighthouse", "lighthouse"},
			by: "team_reboot",
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

	assert.Equal(t, "forbidden: upgradoor:team_hold does not cover host bad??[2Jhost??"+
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
			services: "+upgradoor:team_roll", teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_roll does not cover container lighthouse " +
				"on host lighthouse-geth-1 (owners: lighthouse)",
		},
		{
			name: "the team of the second entry, host-level", action: "reboot",
			body:     `{"hosts":["` + controllertest.HostMixed + `"]}`,
			services: "+upgradoor:team_reboot", teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_reboot does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "the team of the first entry", action: "roll", body: roll,
			services: "+upgradoor:team_roll", teams: []string{"lighthouse"}, by: "team_roll",
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

	require.NoError(t, st.Commit(ctx, &store.Changes{Rollouts: []*fleet.Rollout{
		controllertest.RolloutRow("r-aborted", fleet.RolloutAborted, map[string]fleet.HostState{
			controllertest.HostTeku: fleet.HostPending,
		}),
		// Halted: a host of lighthouse and geth failed, and one of teku and
		// geth; the bootnode waits.
		controllertest.RolloutRow("r-halted", fleet.RolloutHalted, map[string]fleet.HostState{
			controllertest.HostMixed: fleet.HostFailed, controllertest.HostTeku: fleet.HostFailed, controllertest.HostUnowned: fleet.HostPending,
		}),
	}}))

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

	lighthouse := provider.Grant(t, "-all,+upgradoor,+upgradoor:team_recover:team_hold", "lighthouse")
	teku := provider.Grant(t, "-all,+upgradoor,+upgradoor:team_*", "teku")
	supervisor := provider.Grant(t, "+upgradoor:supervise")
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
	assert.Contains(t, msg, "no host could be retried: bootnode-1 has neither failed nor been skipped")

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

	host, err := st.GetHost(ctx, controllertest.HostTeku)
	require.NoError(t, err)
	assert.Equal(t, fleet.TeamHold([]string{"teku"}), host.HoldBy)

	ok("unhold", `{"hosts":["`+controllertest.HostTeku+`"]}`, teku)

	// Staff's hold: the twin does not lift it and does not take it over.
	ok("hold", `{"hosts":["`+controllertest.HostMixed+`"],"reason":"incident"}`, staff)

	for action, verb := range map[string]string{"unhold": "lift", "hold": "change"} {
		code, msg = send(action, `{"hosts":["`+controllertest.HostMixed+`"]}`, lighthouse)
		assert.Equal(t, http.StatusForbidden, code)
		assert.Equal(t, "forbidden: upgradoor:team_hold does not "+verb+
			" the hold on host lighthouse-geth-1 (set by staff)", msg)
	}

	host, err = st.GetHost(ctx, controllertest.HostMixed)
	require.NoError(t, err)
	assert.True(t, host.HoldWanted)
	assert.Equal(t, "incident", host.HoldReason)
	assert.Empty(t, host.HoldBy)

	rec := h.serve(userRoute{method: http.MethodGet, path: "/api/v1/hosts/" + controllertest.HostMixed}.request(lighthouse))
	assert.Equal(t, "staff", controllertest.DecodeJSON[views.HostDetail](t, rec).HoldBy)

	ok("unhold", `{"hosts":["`+controllertest.HostMixed+`"]}`, staff)

	// Through the twin a roll is staggered, whatever was asked.
	rollToken := provider.Grant(t, "+upgradoor:team_roll", "lighthouse")
	res = ok("roll", `{"hosts":["`+controllertest.HostMixed+`"],"containers":["lighthouse"],"staggered":false}`, rollToken)
	assert.Contains(t, res.Message, "staggered")
	assert.False(t, rollout(res.Rollout).Force)

	res = ok("roll", `{"hosts":["`+controllertest.HostMixed+`"],"containers":["lighthouse"]}`, staff)
	assert.Contains(t, res.Message, "now")
	assert.True(t, rollout(res.Rollout).Force)
}

// TestAuditNamesWhatPermittedIt: admin is recorded whenever it is what
// permitted the request as it was executed: the flags, the selector all, and
// "now" for a login that acts through the team twin.
func TestAuditNamesWhatPermittedIt(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.OwnershipFixture(h.store)

	roll := func(extra string) string {
		return `{"hosts":["` + controllertest.HostMixed + `"],"containers":["lighthouse"]` + extra + `}`
	}
	reboot := func(extra string) string { return `{"hosts":["` + controllertest.HostMixed + `"]` + extra + `}` }
	lighthouse := []string{"lighthouse"}

	for _, tt := range []authzCase{
		{
			name: "roll now, twin and admin", action: "roll", body: roll(""),
			services: "+upgradoor:team_roll:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll now, every twin and admin", action: "roll", body: roll(`,"staggered":false`),
			services: "+upgradoor:team_*:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll staggered, twin and admin", action: "roll", body: roll(`,"staggered":true`),
			services: "+upgradoor:team_roll:admin", teams: lighthouse, by: "team_roll",
		},
		{
			name: "roll now, twin alone: run staggered", action: "roll", body: roll(""),
			services: "+upgradoor:team_roll", teams: lighthouse, by: "team_roll",
		},
		{
			name: "roll staggered with force, twin and admin: named once", action: "roll",
			body:     roll(`,"staggered":true,"force":true`),
			services: "+upgradoor:team_roll:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll now with ignoreBudget, twin and admin: named once", action: "roll",
			body:     roll(`,"ignoreBudget":true`),
			services: "+upgradoor:team_roll:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll now, plain and admin: now is the plain feature's", action: "roll", body: roll(""),
			services: "+upgradoor:roll:admin", teams: lighthouse, by: "roll",
		},
		{
			name: "roll now, everything", action: "roll", body: roll(""),
			services: "+upgradoor:all", teams: lighthouse, by: "roll",
		},
		{
			name: "reboot now, twin and admin", action: "reboot", body: reboot(""),
			services: "+upgradoor:team_reboot:admin", teams: lighthouse, by: "team_reboot,admin",
		},
		{
			name: "reboot staggered, twin and admin", action: "reboot", body: reboot(`,"staggered":true`),
			services: "+upgradoor:team_reboot:admin", teams: lighthouse, by: "team_reboot",
		},
		{
			name: "hold, twin and admin: admin permitted nothing", action: "hold", body: reboot(""),
			services: "+upgradoor:team_hold:admin", teams: lighthouse, by: "team_hold",
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
		f.ActionErr = fmt.Errorf("%w: team_hold does not lift the hold on host %s (set by staff)",
			scheduler.ErrForbidden, controllertest.HostMixed)
	})

	req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/x",
		strings.NewReader(`{"hosts":["`+controllertest.HostMixed+`"]}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.srv.actions()["unhold"](rec, req, &authz.User{
		Actor: controllertest.UserEmail, Services: "+upgradoor-dev:team_hold", Teams: []string{"geth"},
	})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, httpjson.ErrorBody{
		Error: "forbidden: upgradoor-dev:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)",
	}, controllertest.DecodeJSON[httpjson.ErrorBody](t, rec))
}

// TestEndLeaseLeavesATeamsHoldTheTeams goes through the real handlers, token
// verification, scheduler and store: ending a lease writes no hold and takes
// none over. A team member who holds the team's host and ends its lease can
// lift that hold afterwards: it is still the team's.
func TestEndLeaseLeavesATeamsHoldTheTeams(t *testing.T) {
	h, provider, st := realHarness(t)
	ctx := context.Background()

	syncOwned(t, st, controllertest.HostMixed, controllertest.Owned("geth", protocol.KindEL, "geth"), controllertest.Owned("lighthouse", protocol.KindCL, "lighthouse"))

	require.NoError(t, st.Commit(ctx, &store.Changes{Leases: []*fleet.Lease{{
		ID: "lease-1", Host: controllertest.HostMixed, State: fleet.LeaseRunning, GrantedBy: "scheduler",
		Needs:     []protocol.Need{{ID: "reboot", Kind: protocol.NeedReboot}},
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
	require.Equal(t, fleet.TeamHold([]string{"lighthouse"}), host().HoldBy)

	rec = send("end-lease", `{"lease":"lease-1"}`, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.True(t, host().HoldWanted)
	assert.Equal(t, "ours", host().HoldReason)
	assert.Equal(t, fleet.TeamHold([]string{"lighthouse"}), host().HoldBy, "ending a lease changes no hold")

	entries, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action.end-lease"})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.NotContains(t, entries[0].Detail, "hold")

	rec = h.serve(userRoute{method: http.MethodGet, path: "/api/v1/hosts/" + controllertest.HostMixed}.request(team))
	assert.Equal(t, fleet.TeamHold([]string{"lighthouse"}), controllertest.DecodeJSON[views.HostDetail](t, rec).HoldBy)

	// The team lifts its hold, while the lease is still open.
	rec = send("unhold", hosts, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, host().HoldWanted)

	// Lifting it again changes nothing and is not audited.
	before, err := st.ListAudit(ctx, store.AuditFilter{ActionPrefix: "action."})
	require.NoError(t, err)

	rec = send("unhold", hosts, team)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, controllertest.DecodeJSON[scheduler.ActionResult](t, rec).Message, "1 host(s) were not held")

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

		admin := authzCase{refusal: lacks}
		if s.feature == authz.FeatureAdmin {
			admin = authzCase{by: authz.FeatureAdmin}
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
			{name: "upgradoor:all", services: "+upgradoor:all", by: s.feature},
			{name: "upgradoor:all, twin spelled too", services: "+upgradoor:all:" + twin, by: s.feature},
			{name: "all:all", services: "+all:all", teams: strangers, by: s.feature},
			{name: "only upgradoor:all", services: "-all,+upgradoor:all", by: s.feature},
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
			// "all" without the plain feature still holds the twin, which
			// covers nothing for a login without teams.
			{
				name: "all but this feature", services: "+upgradoor:all:-" + s.feature,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{
				name: "all, this feature removed later", services: "+all:all,-upgradoor:" + s.feature,
				refusal: notCovered.refusal, refusalPrefix: notCovered.refusalPrefix,
			},
			{
				name:     "all but this feature and its twin",
				services: "+upgradoor:all:-" + s.feature + ":-" + twin, teams: owners, refusal: lacks,
			},
			{
				name: "admin alone implies nothing", services: "+upgradoor:admin", teams: owners,
				by: admin.by, refusal: admin.refusal,
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

// TestAuthorizeTargets: what the team twins cover, and what needs admin.
func TestAuthorizeTargets(t *testing.T) {
	hosts := func(names ...string) string {
		raw, err := json.Marshal(names)
		require.NoError(t, err)

		return string(raw)
	}

	teamRoll, teamReboot := "+upgradoor:team_roll", "+upgradoor:team_reboot"
	teamRecover := "+upgradoor:team_recover"
	lighthouse, geth := []string{"lighthouse"}, []string{"geth"}
	both := []string{"geth", "lighthouse"}

	rollMixed := func(containers string) string {
		return `{"hosts":` + hosts(controllertest.HostMixed) + `,"containers":` + containers + `}`
	}

	tests := []authzCase{
		// A roll with named containers: every one of them.
		{
			name: "roll, own container", action: "roll", body: rollMixed(`["lighthouse"]`),
			services: teamRoll, teams: lighthouse, by: "team_roll",
		},
		{
			name: "roll, own containers", action: "roll",
			body:     rollMixed(`["lighthouse","lighthouse-validator"]`),
			services: teamRoll, teams: lighthouse, by: "team_roll",
		},
		{
			name: "roll, mixed ownership", action: "roll", body: rollMixed(`["lighthouse","geth"]`),
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll, mixed ownership, in both teams", action: "roll",
			body:     rollMixed(`["lighthouse","geth"]`),
			services: teamRoll, teams: both, by: "team_roll",
		},
		{
			name: "roll, the other team's container", action: "roll", body: rollMixed(`["geth"]`),
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll, a container of its team", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostTeku) + `,"containers":["teku"]}`,
			services: teamRoll, teams: []string{"teku"}, by: "team_roll",
		},
		{
			name: "roll, a container of another team", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostTeku) + `,"containers":["teku"]}`,
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_roll does not cover container teku " +
				"on host teku-geth-1 (owners: teku)",
		},
		{
			name: "roll, one container on two hosts", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostTeku) + `,"containers":["geth"]}`,
			services: teamRoll, teams: geth, by: "team_roll",
		},
		{
			name: "roll, a container one host does not have", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostUnowned) + `,"containers":["geth"]}`,
			services: teamRoll, teams: geth,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "roll, unknown container", action: "roll", body: rollMixed(`["nethermind"]`),
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container nethermind " +
				"on host lighthouse-geth-1 (owners: none)",
		},
		{
			name: "roll, unmanaged container", action: "roll", body: rollMixed(`["debug"]`),
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container debug " +
				"on host lighthouse-geth-1 (owners: none)",
		},
		{
			name: "roll, unknown container, plain feature", action: "roll",
			body: rollMixed(`["nethermind"]`), services: "+upgradoor:roll", by: "roll",
		},

		// A roll without containers: every managed container of the host.
		{
			name: "roll a host, one team of two", action: "roll", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host lighthouse-geth-1 (owners: geth)",
		},
		{
			name: "roll a host, both teams", action: "roll", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamRoll, teams: both, by: "team_roll",
		},
		{
			name: "roll hosts, second not covered", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostTeku) + `}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container teku " +
				"on host teku-geth-1 (owners: teku)",
		},
		{
			name: "roll a host nobody owns", action: "roll", body: `{"hosts":` + hosts(controllertest.HostUnowned) + `}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container bootnode " +
				"on host bootnode-1 (owners: none)",
		},
		{
			name: "roll a host that never synced", action: "roll", body: `{"hosts":` + hosts(controllertest.HostFresh) + `}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover host fresh-1 (owners: none)",
		},
		{
			name: "roll an unknown host", action: "roll", body: `{"hosts":` + hosts(controllertest.HostGhost) + `}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover host ghost-1 (owners: none)",
		},
		{
			name: "roll an unknown host, named container", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostGhost) + `,"containers":["geth"]}`,
			services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll does not cover container geth " +
				"on host ghost-1 (owners: none)",
		},
		{
			// The scheduler answers the unknown host.
			name: "roll an unknown host, plain feature", action: "roll",
			body: `{"hosts":` + hosts(controllertest.HostGhost) + `}`, services: "+upgradoor:roll", by: "roll",
		},
		{
			name: "roll, no target", action: "roll", body: `{}`, services: teamRoll, teams: both,
			refusal: "forbidden: upgradoor:team_roll needs a target that one of your teams owns, " +
				"and the request names none",
		},

		// Host-level actions: one owned container is enough.
		{
			name: "reboot, the CL team", action: "reboot", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamReboot, teams: lighthouse, by: "team_reboot",
		},
		{
			name: "reboot, the EL team", action: "reboot", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamReboot, teams: geth, by: "team_reboot",
		},
		{
			name: "reboot, a team without a container there", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamReboot, teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_reboot does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "reboot two hosts, the EL team", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostTeku) + `}`,
			services: teamReboot, teams: geth, by: "team_reboot",
		},
		{
			name: "reboot two hosts, second not covered", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed, controllertest.HostTeku) + `}`,
			services: teamReboot, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_reboot does not cover host teku-geth-1 " +
				"(owners: geth, teku)",
		},
		{
			name: "reboot a host nobody owns", action: "reboot", body: `{"hosts":` + hosts(controllertest.HostUnowned) + `}`,
			services: teamReboot, teams: both,
			refusal: "forbidden: upgradoor:team_reboot does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "reboot a host that never synced", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostFresh) + `}`,
			services: teamReboot, teams: both,
			refusal: "forbidden: upgradoor:team_reboot does not cover host fresh-1 (owners: none)",
		},
		{
			name: "reboot an unknown host", action: "reboot", body: `{"hosts":` + hosts(controllertest.HostGhost) + `}`,
			services: teamReboot, teams: both,
			refusal: "forbidden: upgradoor:team_reboot does not cover host ghost-1 (owners: none)",
		},
		{
			name: "reboot an unknown host, plain feature", action: "reboot",
			body: `{"hosts":` + hosts(controllertest.HostGhost) + `}`, services: "+upgradoor:reboot", by: "reboot",
		},
		{
			name: "hold, no target", action: "hold", body: `{"hosts":[]}`,
			services: "+upgradoor:team_hold", teams: both,
			refusal: "forbidden: upgradoor:team_hold needs a target that one of your teams owns, " +
				"and the request names none",
		},
		{
			name: "unhold, a host nobody owns", action: "unhold", body: `{"hosts":` + hosts(controllertest.HostUnowned) + `}`,
			services: "+upgradoor:team_hold", teams: both,
			refusal: "forbidden: upgradoor:team_hold does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "the twin of another feature", action: "hold", body: `{"hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamReboot, teams: both,
			refusal: "forbidden: this login lacks the feature upgradoor:hold",
		},

		// The flags and the selector all need admin on top.
		{
			name: "reboot with force, no admin", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `,"force":true}`,
			services: "+upgradoor:reboot",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin (needed for force)",
		},
		{
			name: "reboot with force, admin", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `,"force":true}`,
			services: "+upgradoor:reboot:admin", by: "reboot,admin",
		},
		{
			name: "reboot with force, admin without reboot", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `,"force":true}`,
			services: "+upgradoor:admin",
			refusal:  "forbidden: this login lacks the feature upgradoor:reboot",
		},
		{
			name: "reboot with force, team twin and admin", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `,"force":true}`,
			services: "+upgradoor:team_reboot:admin", teams: geth, by: "team_reboot,admin",
		},
		{
			name: "reboot with force, team twin without admin", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `,"force":true}`,
			services: teamReboot, teams: geth,
			refusal: "forbidden: this login lacks the feature upgradoor:admin (needed for force)",
		},
		{
			name: "reboot with force, team twin and admin, not covered", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostUnowned) + `,"force":true}`,
			services: "+upgradoor:team_reboot:admin", teams: geth,
			refusal: "forbidden: upgradoor:team_reboot does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "reboot with force, everything", action: "reboot",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `,"force":true}`,
			services: "+upgradoor:all", by: "reboot,admin",
		},
		{
			name: "roll with ignoreBudget, no admin", action: "roll",
			body:     rollMixed(`["lighthouse"]`)[:len(rollMixed(`["lighthouse"]`))-1] + `,"ignoreBudget":true}`,
			services: teamRoll, teams: lighthouse,
			refusal: "forbidden: this login lacks the feature upgradoor:admin (needed for ignoreBudget)",
		},
		{
			name: "roll with ignoreBudget, admin", action: "roll",
			body:     rollMixed(`["lighthouse"]`)[:len(rollMixed(`["lighthouse"]`))-1] + `,"ignoreBudget":true}`,
			services: "+upgradoor:team_*:admin", teams: lighthouse, by: "team_roll,admin",
		},
		{
			name: "roll with force, plain", action: "roll", body: `{"hosts":` + hosts(controllertest.HostMixed) + `,"force":true}`,
			services: "+upgradoor:roll",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin (needed for force)",
		},
		{
			name: "roll, staggered is no flag", action: "roll",
			body:     `{"hosts":` + hosts(controllertest.HostMixed) + `,"staggered":true}`,
			services: "+upgradoor:roll", by: "roll",
		},
		{
			name: "roll all, plain", action: "roll", body: `{"all":true}`, services: "+upgradoor:roll",
			refusal: "forbidden: this login lacks the feature upgradoor:admin (needed for all)",
		},
		{
			name: "roll all, admin", action: "roll", body: `{"all":true}`,
			services: "+upgradoor:roll:admin", by: "roll,admin",
		},
		{
			name: "roll all, team twin and admin: not every host is covered", action: "roll",
			body: `{"all":true}`, services: "+upgradoor:team_roll:admin", teams: both,
			refusalPrefix: "forbidden: upgradoor:team_roll does not cover ",
		},
		{
			name: "reboot all, plain", action: "reboot", body: `{"all":true}`, services: "+upgradoor:reboot",
			refusal: "forbidden: this login lacks the feature upgradoor:admin (needed for all)",
		},
		{
			name: "reboot all, admin", action: "reboot", body: `{"all":true,"staggered":true}`,
			services: "+upgradoor:reboot:admin", by: "reboot,admin",
		},

		// One rollout, or everything.
		{
			name: "pause one rollout with admin only", action: "pause", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:admin",
			refusal:  "forbidden: this login lacks the feature upgradoor:supervise",
		},
		{
			name: "pause everything with supervise only", action: "pause", body: `{}`,
			services: "+upgradoor:supervise",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin",
		},
		{
			name: "resume everything with supervise only", action: "resume", body: `{}`,
			services: "+upgradoor:supervise",
			refusal:  "forbidden: this login lacks the feature upgradoor:admin",
		},
		{
			name: "abort with a team twin", action: "abort", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:team_*", teams: both,
			refusal: "forbidden: this login lacks the feature upgradoor:supervise",
		},

		// Retry: hosts are a recovery, none is the whole rollout.
		{
			name: "retry hosts, team", action: "retry",
			body:     `{"rollout":"r-1","hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: teamRecover, teams: geth, by: "team_recover",
		},
		{
			name: "retry hosts, team, not covered", action: "retry",
			body:     `{"rollout":"r-1","hosts":` + hosts(controllertest.HostMixed, controllertest.HostUnowned) + `}`,
			services: teamRecover, teams: geth,
			refusal: "forbidden: upgradoor:team_recover does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "retry the rollout, team", action: "retry", body: `{"rollout":"r-1"}`,
			services: teamRecover, teams: both,
			refusal: "forbidden: this login lacks the feature upgradoor:supervise",
		},
		{
			name: "retry the rollout, recover", action: "retry", body: `{"rollout":"r-1"}`,
			services: "+upgradoor:recover",
			refusal:  "forbidden: this login lacks the feature upgradoor:supervise",
		},
		{
			name: "retry hosts, rollouts", action: "retry",
			body:     `{"rollout":"r-1","hosts":` + hosts(controllertest.HostMixed) + `}`,
			services: "+upgradoor:supervise",
			refusal:  "forbidden: this login lacks the feature upgradoor:recover",
		},
		{
			name: "skip, team, not covered", action: "skip",
			body:     `{"rollout":"r-1","hosts":` + hosts(controllertest.HostTeku) + `}`,
			services: teamRecover, teams: lighthouse,
			refusal: "forbidden: upgradoor:team_recover does not cover host teku-geth-1 " +
				"(owners: geth, teku)",
		},

		// A lease stands for its host.
		{
			name: "end a lease of an owned host", action: "end-lease", body: `{"lease":"` + controllertest.LeaseMixed + `"}`,
			services: teamRecover, teams: geth, by: "team_recover",
		},
		{
			name: "end a closed lease of an owned host", action: "end-lease",
			body: `{"lease":"` + controllertest.LeaseDone + `"}`, services: teamRecover, teams: geth, by: "team_recover",
		},
		{
			name: "end a lease of another team's host", action: "end-lease",
			body:     `{"lease":"` + controllertest.LeaseMixed + `"}`,
			services: teamRecover, teams: []string{"teku"},
			refusal: "forbidden: upgradoor:team_recover does not cover host lighthouse-geth-1 " +
				"(owners: geth, lighthouse)",
		},
		{
			name: "end a lease of a host nobody owns", action: "end-lease",
			body:     `{"lease":"` + controllertest.LeaseUnowned + `"}`,
			services: teamRecover, teams: both,
			refusal: "forbidden: upgradoor:team_recover does not cover host bootnode-1 (owners: none)",
		},
		{
			name: "end an unknown lease", action: "end-lease", body: `{"lease":"` + controllertest.LeaseGhost + `"}`,
			services: teamRecover, teams: both,
			refusal: "forbidden: upgradoor:team_recover does not cover lease lease-ghost (owners: none)",
		},
		{
			name: "end an unknown lease, plain feature", action: "end-lease",
			body: `{"lease":"` + controllertest.LeaseGhost + `"}`, services: "+upgradoor:recover", by: "recover",
		},
		{
			name: "end no lease", action: "end-lease", body: `{}`, services: teamRecover, teams: both,
			refusal: "forbidden: upgradoor:team_recover needs a target that one of your teams owns, " +
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
			refusal: "forbidden: this login lacks the feature upgradoor-dev:hold",
		},
		{name: "the configured name", services: "+upgradoor-dev:hold", by: "hold"},
		{name: "all services", services: "+all:all", by: "hold"},
		{
			name: "the team twin", services: "+upgradoor-dev:team_hold", teams: []string{"geth"},
			by: "team_hold",
		},
		{
			name: "the team twin, not covered", services: "+upgradoor-dev:team_hold",
			teams: []string{"teku"},
			refusal: "forbidden: upgradoor-dev:team_hold does not cover host lighthouse-geth-1 " +
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
