package userapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// actionFixture: the mixed and the teku host report the action, which the
// feature upgradoor:actions and the team devops may start; the unowned host
// does not have it.
func actionFixture(h *harness) {
	controllertest.OwnershipFixture(h.store)

	h.store.Lock()
	defer h.store.Unlock()

	for _, name := range []string{controllertest.HostMixed, controllertest.HostTeku} {
		h.store.Hosts[name].Status.Actions = []protocol.ActionInfo{{
			Name: "security-upgrades", Impact: protocol.ImpactIndependent,
			Feature: "upgradoor:actions", Teams: []string{"devops"},
		}}
	}

	h.store.Rollouts = append(h.store.Rollouts, &fleet.Rollout{
		ID: "r-action", Kind: fleet.RolloutAction, Action: "security-upgrades", Key: "action:security-upgrades",
		State: fleet.RolloutActive, Hosts: []fleet.RolloutHost{{Host: controllertest.HostTeku}},
	})
}

func postAction(h *harness, u *authz.User, name, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+"/api/v1/actions/"+name, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.srv.actions()[name](rec, req, u)

	return rec
}

// TestHostActionPermissions: admin, the action's feature or one of its teams
// permit a host; a selection by client or all takes the permitted hosts that
// have the action, a named host that is not permitted is rejected.
func TestHostActionPermissions(t *testing.T) {
	user := func(services string, teams ...string) *authz.User {
		return &authz.User{Actor: controllertest.UserEmail, Services: services, Teams: teams}
	}

	tests := []struct {
		name     string
		user     *authz.User
		body     string
		status   int
		hosts    []string
		basis    string
		rejected map[string]string
	}{
		{
			name: "admin, all hosts", user: user("+upgradoor:admin"), body: `{"action":"security-upgrades","all":true}`,
			status: http.StatusOK, hosts: []string{controllertest.HostMixed, controllertest.HostTeku}, basis: "admin",
		},
		{
			name: "the action's feature", user: user("+upgradoor:actions"),
			body:   `{"action":"security-upgrades","hosts":["` + controllertest.HostTeku + `"]}`,
			status: http.StatusOK, hosts: []string{controllertest.HostTeku}, basis: "feature",
		},
		{
			name: "the action's team, by client", user: user("+upgradoor", "devops"),
			body:   `{"action":"security-upgrades","client":"teku"}`,
			status: http.StatusOK, hosts: []string{controllertest.HostTeku}, basis: "team",
		},
		{
			name: "a named host without the action", user: user("+upgradoor", "devops"),
			body:   `{"action":"security-upgrades","hosts":["` + controllertest.HostTeku + `","` + controllertest.HostUnowned + `"]}`,
			status: http.StatusOK, hosts: []string{controllertest.HostTeku, controllertest.HostUnowned}, basis: "team",
		},
		{
			name: "no permission", user: user("+upgradoor:roll", "geth"),
			body: `{"action":"security-upgrades","all":true}`, status: http.StatusForbidden,
		},
		{
			name: "two selections", user: user("+upgradoor:admin"),
			body: `{"action":"security-upgrades","all":true,"client":"teku"}`, status: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			actionFixture(h)

			rec := postAction(h, tt.user, "host-action", tt.body)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())

			calls := h.sched.Recorded()
			if tt.status != http.StatusOK {
				assert.Empty(t, calls)

				return
			}

			require.Len(t, calls, 1)
			req, ok := calls[0].Req.(scheduler.HostActionRequest)
			require.True(t, ok)
			assert.Equal(t, tt.hosts, req.Hosts)
			assert.Equal(t, "action", calls[0].Auth.By)

			for _, host := range req.Hosts {
				if host == controllertest.HostUnowned {
					assert.Empty(t, req.Basis[host], "the scheduler refuses it")

					continue
				}

				assert.Equal(t, tt.basis, req.Basis[host], host)
			}
		})
	}
}

// A login that may run the action on every member host approves its
// rollout; any other resume needs supervise as before.
func TestResumeApprovesActionRollouts(t *testing.T) {
	h := newHarness(t, nil)
	actionFixture(h)

	team := &authz.User{Actor: controllertest.UserEmail, Services: "+upgradoor", Teams: []string{"devops"}}

	rec := postAction(h, team, "resume", `{"rollout":"r-action"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	calls := h.sched.Recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, "action:team", calls[0].Auth.By)

	rec = postAction(h, team, "resume", `{"rollout":"r-1"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	stranger := &authz.User{Actor: controllertest.UserEmail, Services: "+upgradoor", Teams: []string{"geth"}}
	rec = postAction(h, stranger, "resume", `{"rollout":"r-action"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}
