package userapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func approver(services string, teams ...string) *authz.User {
	return &authz.User{Actor: controllertest.UserEmail, Services: services, Teams: teams}
}

// Whoever may approve a rollout lets it pass the gate: manage, its team's
// twin, or the action on every member of an action rollout. Admin is not
// needed.
func TestIgnoreGatePermissions(t *testing.T) {
	tests := []struct {
		name   string
		user   *authz.User
		body   string
		status int
		by     string
	}{
		{
			name: "team twin, own rollout", user: approver("+upgradoor:team_manage", "lighthouse"),
			body: `{"rollout":"r-lighthouse","reason":"beacon down"}`, status: http.StatusOK, by: "team_manage",
		},
		{
			name: "team twin, rollout of no team", user: approver("+upgradoor:team_manage", "lighthouse"),
			body: `{"rollout":"r-1"}`, status: http.StatusForbidden,
		},
		{
			name: "manage", user: approver("+upgradoor:manage"),
			body: `{"rollout":"r-1"}`, status: http.StatusOK, by: "manage",
		},
		{
			name: "the action's team", user: approver("+upgradoor", "devops"),
			body: `{"rollout":"r-action"}`, status: http.StatusOK, by: "action:team",
		},
		{
			name: "no rollout", user: approver("+upgradoor:manage:admin"),
			body: `{}`, status: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			actionFixture(h)

			rec := postAction(h, tt.user, "ignore-gate", tt.body)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())

			calls := h.sched.Recorded()
			if tt.status != http.StatusOK {
				assert.Empty(t, calls)

				return
			}

			require.Len(t, calls, 1)
			assert.Equal(t, "ignore-gate", calls[0].Name)
			assert.Equal(t, tt.by, calls[0].Auth.By)
		})
	}
}

// Approving a lease takes the approval of every rollout the lease serves.
func TestApproveLeasePermissions(t *testing.T) {
	team := approver("+upgradoor:team_manage", "lighthouse")
	body := `{"rollout":"r-lighthouse","host":"` + controllertest.HostMixed + `","reason":"no liveness"}`

	setup := func(t *testing.T, rollouts map[string]string) *harness {
		t.Helper()

		h := newHarness(t, nil)
		controllertest.OwnershipFixture(h.store)

		h.store.Lock()
		defer h.store.Unlock()

		h.store.Rollouts[2].Hosts = []fleet.RolloutHost{{
			Host: controllertest.HostMixed, State: fleet.HostLeased, LeaseID: controllertest.LeaseMixed,
		}}
		h.store.Leases[0].State = fleet.LeaseVerifying
		h.store.Leases[0].Rollouts = rollouts

		return h
	}

	t.Run("one rollout of the team", func(t *testing.T) {
		h := setup(t, map[string]string{"n1": "r-lighthouse", "n2": "r-lighthouse"})

		rec := postAction(h, team, "approve-lease", body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		calls := h.sched.Recorded()
		require.Len(t, calls, 1)
		assert.Equal(t, scheduler.ApproveLeaseRequest{
			Rollout: "r-lighthouse", Host: controllertest.HostMixed, Reason: "no liveness",
		}, calls[0].Req)
		assert.Equal(t, "team_manage", calls[0].Auth.By)
	})

	t.Run("another rollout the team may not approve", func(t *testing.T) {
		h := setup(t, map[string]string{"n1": "r-lighthouse", "n2": "r-1"})

		rec := postAction(h, team, "approve-lease", body)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "r-1")
		assert.Empty(t, h.sched.Recorded())

		rec = postAction(h, approver("+upgradoor:manage"), "approve-lease", body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	// A manual roll of the team's containers records the team, and is the
	// team's to approve like its client's rollout.
	t.Run("a manual roll of the team's containers", func(t *testing.T) {
		h := setup(t, map[string]string{"n1": "r-lighthouse", "n2": "r-manual"})

		h.store.Lock()
		h.store.Rollouts = append(h.store.Rollouts, &fleet.Rollout{
			ID: "r-manual", Kind: fleet.RolloutManual, ContainerKind: protocol.KindCL, Team: "lighthouse",
			State: fleet.RolloutActive, Hosts: h.store.Rollouts[2].Hosts,
		})
		h.store.Unlock()

		rec := postAction(h, team, "approve-lease", body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = postAction(h, team, "approve-lease",
			`{"rollout":"r-manual","host":"`+controllertest.HostMixed+`","reason":"no liveness"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = postAction(h, approver("+upgradoor:team_manage", "geth"), "approve-lease", body)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("a host is required", func(t *testing.T) {
		h := setup(t, nil)

		rec := postAction(h, team, "approve-lease", `{"rollout":"r-lighthouse"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
}
