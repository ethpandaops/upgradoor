package fleet

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditActorKindOf(t *testing.T) {
	t.Parallel()

	for actor, want := range map[string]AuditActorKind{
		"scheduler":         AuditScheduler,
		"user:scheduler":    AuditPeople,
		"agent:node-1":      AuditAgents,
		"agent":             AuditPeople,
		"ssh:github:pk910":  AuditSSH,
		"ssh:SHA256:abc":    AuditSSH,
		"alice@example.org": AuditPeople,
		"anonymous":         AuditPeople,
		"":                  AuditPeople,
	} {
		assert.Equal(t, want, AuditActorKindOf(actor), actor)
	}
}

func TestAuditActions(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, len(AuditActions))

	for _, a := range AuditActions {
		assert.False(t, seen[a.Action], "listed twice: %s", a.Action)
		seen[a.Action] = true

		assert.NotEmpty(t, a.Group, a.Action)
		assert.NotEmpty(t, a.Target, a.Action)

		got, ok := AuditActionOf(a.Action)
		require.True(t, ok, a.Action)
		assert.Equal(t, a, got)
	}

	_, ok := AuditActionOf("nope")
	assert.False(t, ok)

	leases, dense := AuditGroupActions(AuditGroupLeases)
	assert.True(t, dense, "the scheduler grants leases")
	assert.Contains(t, leases, "action.end-lease")

	holds, dense := AuditGroupActions(AuditGroupHolds)
	assert.False(t, dense, "only people hold hosts")
	assert.Equal(t, []string{"action.hold", "action.unhold", "action.mark-offline", "action.unmark-offline"}, holds)

	rollouts, _ := AuditTargetActions(AuditTargetRollout)
	assert.Contains(t, rollouts, "action.host-action")
	assert.NotContains(t, rollouts, "lease.granted")
}
