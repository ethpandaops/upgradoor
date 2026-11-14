package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
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
			h.sched.set(func(f *fakeScheduler) { f.result = result })

			rec := h.sendJSON(http.MethodPost, "/api/v1/actions/"+tt.action, tt.body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, *result, decodeJSON[scheduler.ActionResult](t, rec))

			calls := h.sched.recorded()
			require.Len(t, calls, 1)
			assert.Equal(t, schedulerCall{Name: tt.action, Actor: actorAnonymous, Req: tt.want},
				calls[0])
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
			assert.Contains(t, decodeJSON[ErrorBody](t, rec).Error, tt.contains)
			assert.Empty(t, h.sched.recorded(), "an invalid action never reaches the scheduler")
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
				h.sched.set(func(f *fakeScheduler) { f.actionErr = tt.err })

				rec := h.sendJSON(http.MethodPost, "/api/v1/actions/"+action, body)
				require.Equal(t, tt.status, rec.Code, rec.Body.String())
				assert.Equal(t, ErrorBody{Error: tt.want}, decodeJSON[ErrorBody](t, rec))
			})
		}
	}
}

func TestActionRequestShape(t *testing.T) {
	h := newHarness(t, nil)

	// Unknown action.
	rec := h.sendJSON(http.MethodPost, "/api/v1/actions/explode", `{}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"))

	// Wrong method on a known action.
	rec = h.get("/api/v1/actions/roll")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "POST", rec.Header().Get("Allow"))
	assert.Equal(t, ErrorBody{Error: "method not allowed"}, decodeJSON[ErrorBody](t, rec))

	// A body that is not declared as JSON (what an HTML form can send).
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		req := httptest.NewRequest(http.MethodPost, testBaseURL+"/api/v1/actions/roll",
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

	assert.Empty(t, h.sched.recorded())

	// A scheduler that answers nothing still yields a result.
	h.sched.set(func(f *fakeScheduler) { f.result = nil })

	rec = h.sendJSON(http.MethodPost, "/api/v1/actions/pause", `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, scheduler.ActionResult{Message: "ok"},
		decodeJSON[scheduler.ActionResult](t, rec))
}
