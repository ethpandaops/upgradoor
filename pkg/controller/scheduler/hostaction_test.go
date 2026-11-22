package scheduler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const secAction = "security-upgrades"

// withAction makes the host report the action, due or not.
func withAction(st *protocol.SyncRequest, impact protocol.Impact, due bool) {
	st.Verbs = append(st.Verbs, protocol.VerbAction)
	st.Actions = []protocol.ActionInfo{{
		Name: secAction, Impact: impact, When: protocol.ConditionSecurityUpdates, Due: due,
	}}

	if due {
		st.Needs = append(st.Needs, protocol.Need{
			ID: protocol.ActionNeedID(secAction), Kind: protocol.NeedAction,
			Action: &protocol.ActionNeed{Name: secAction, Impact: impact, When: protocol.ConditionSecurityUpdates},
		})
	}
}

// A person's start goes to the permitted hosts that report the action, as
// one staggered rollout with one directive per host naming the action.
func TestHostActionStart(t *testing.T) {
	h := newHarness(t)

	for _, name := range []string{"a", "b", "c", "d"} {
		h.addHost(name, 0)
	}

	for _, name := range []string{"a", "b", "c"} {
		h.sync(name, func(st *protocol.SyncRequest) { withAction(st, protocol.ImpactIndependent, false) })
	}

	res, err := h.svc.HostAction(h.ctx, "alice", HostActionRequest{
		Action: secAction, Hosts: []string{"a", "b", "c", "d"}, PauseAfterCanary: true, Reason: "CVE",
		Basis: map[string]string{"a": "admin", "b": "team", "d": "feature"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, res.Hosts)
	assert.Equal(t, map[string]string{
		"c": "not permitted", "d": "the host has no action " + secAction,
	}, res.Rejected)

	r := h.rollout(res.Rollout)
	assert.Equal(t, fleet.RolloutAction, r.Kind)
	assert.Equal(t, secAction, r.Action)
	assert.Equal(t, prefixManual+r.ID, r.Key)
	assert.True(t, r.Approved)
	assert.True(t, r.PauseAfterCanary)
	assert.False(t, r.Force)

	audit := h.store.lastAuditOf("action.host-action")
	assert.Equal(t, map[string]string{"a": "admin", "b": "team"}, audit.Detail["basis"])

	dirs := h.assignment("b").Directives
	require.Len(t, dirs, 1)
	assert.Equal(t, protocol.DirectiveAction, dirs[0].Kind)
	assert.Equal(t, secAction, dirs[0].Action)
	assert.Equal(t, "action:team", dirs[0].By.Feature)

	_, err = h.svc.HostAction(h.ctx, "alice", HostActionRequest{
		Action: secAction, Hosts: []string{"c"}, Basis: map[string]string{},
	})
	require.ErrorIs(t, err, ErrInvalidAction)
}

// The hosts' reports open one rollout per action. Its policy is manual
// unless actionPolicies says auto; independent work has its own limit and
// passes a closed gate, node work waits for the gate.
func TestActionRolloutPolicyAndImpact(t *testing.T) {
	tests := []struct {
		name    string
		policy  fleet.Policy
		impact  protocol.Impact
		gateBad bool
		leased  bool
		wait    WaitKind
	}{
		{name: "manual by default", impact: protocol.ImpactIndependent, wait: WaitApproval},
		{name: "auto", policy: fleet.PolicyAuto, impact: protocol.ImpactIndependent, leased: true},
		{
			name: "independent passes a closed gate", policy: fleet.PolicyAuto,
			impact: protocol.ImpactIndependent, gateBad: true, leased: true,
		},
		{
			name: "node waits for the gate", policy: fleet.PolicyAuto,
			impact: protocol.ImpactNode, gateBad: true, wait: WaitGate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newWorldBuilder(t)

			if tt.policy != "" {
				b.w.Settings.ActionPolicies = fleet.ActionPolicies{secAction: tt.policy}
			}

			if tt.gateBad {
				b.healthMod = func(n *fleet.NetworkHealth) { n.FinalityLagEpochs = 10 }
				b.refreshHealth()
			}

			withAction(b.host("h1", 10).Status, tt.impact, true)
			out := b.step()

			r := b.mustRollout(prefixAction + secAction)
			assert.Equal(t, fleet.RolloutAction, r.Kind)
			assert.Equal(t, secAction, r.Action)

			l := b.lease("h1")
			if !tt.leased {
				assert.Nil(t, l)
				require.Len(t, out.Plan.Queue, 1)
				assert.Equal(t, tt.wait, out.Plan.Queue[0].Wait)

				return
			}

			require.NotNil(t, l)
			assert.Equal(t, tt.impact, l.Impact)
			assert.Equal(t, 1, out.Plan.Budget.IndependentHosts)
			assert.Zero(t, l.Charged, "independent work costs no budget")
		})
	}
}
