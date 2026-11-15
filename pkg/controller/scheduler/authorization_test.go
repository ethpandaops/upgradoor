package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

func TestStampAuthorization(t *testing.T) {
	shared := map[string]any{keyReason: "debugging"}
	entries := []fleet.AuditEntry{
		{Action: "action.hold", Target: "node-1", Detail: shared},
		{Action: "action.forget", Target: "node-2"},
	}

	// A context without an authorization (the reconcile, a sync) leaves the
	// entries as they are.
	stampAuthorization(context.Background(), entries)
	assert.Equal(t, map[string]any{keyReason: "debugging"}, entries[0].Detail)
	assert.Nil(t, entries[1].Detail)

	teams := []string{"geth", "lighthouse"}
	ctx := WithAuthorization(context.Background(), Authorization{By: "team_hold", Teams: teams})

	got, ok := AuthorizationFrom(ctx)
	require.True(t, ok)
	assert.Equal(t, Authorization{By: "team_hold", Teams: teams}, got)

	stampAuthorization(ctx, entries)

	assert.Equal(t, map[string]any{
		keyReason: "debugging", keyAuthorizedBy: "team_hold", keyTeams: teams,
	}, entries[0].Detail)
	assert.Equal(t, map[string]any{keyAuthorizedBy: "team_hold", keyTeams: teams}, entries[1].Detail)

	// The detail an action built is not written to, and the entry does not
	// share the caller's team list.
	assert.Equal(t, map[string]any{keyReason: "debugging"}, shared)

	teams[0] = "changed"

	assert.Equal(t, []string{"geth", "lighthouse"}, entries[0].Detail[keyTeams])

	// A login without teams is recorded with an empty list, not without one.
	open := []fleet.AuditEntry{{Action: "action.pause"}}
	stampAuthorization(WithAuthorization(context.Background(), Authorization{By: "open"}), open)
	assert.Equal(t, map[string]any{keyAuthorizedBy: "open", keyTeams: []string{}}, open[0].Detail)
}
