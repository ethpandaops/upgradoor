package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// TestHoldBy: a hold remembers that it was set through a team feature only
// when the writer says so. Every writer that does not (SetHostHold, a
// Changes.Holds entry without By: the scheduler, the end of a lease) makes it
// a hold of staff, also over a team's hold. A lifted hold remembers nothing.
func TestHoldBy(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			addHost(t, s, "node-1", nil)

			by := func() (wanted bool, reason, by string) {
				h, err := s.GetHost(ctx, "node-1")
				require.NoError(t, err)

				// The list reads the same column.
				hosts, err := s.ListHosts(ctx)
				require.NoError(t, err)
				require.Len(t, hosts, 1)
				assert.Equal(t, h.HoldBy, hosts[0].HoldBy)

				return h.HoldWanted, h.HoldReason, h.HoldBy
			}

			commit := func(hold HostHold) {
				require.NoError(t, s.Commit(ctx, &Changes{Holds: []HostHold{hold}}))
			}

			_, _, who := by()
			assert.Empty(t, who, "a new host")

			commit(HostHold{Host: "node-1", Wanted: true, Reason: "ours", By: fleet.HoldByTeam})
			wanted, reason, who := by()
			assert.True(t, wanted)
			assert.Equal(t, "ours", reason)
			assert.Equal(t, fleet.HoldByTeam, who)

			// The end of a lease extends the reason through Changes.Holds
			// without By: the hold is staff's from then on.
			commit(HostHold{Host: "node-1", Wanted: true, Reason: "ours; lease ended by operator"})
			_, reason, who = by()
			assert.Equal(t, "ours; lease ended by operator", reason)
			assert.Empty(t, who)

			commit(HostHold{Host: "node-1", Wanted: true, Reason: "ours", By: fleet.HoldByTeam})
			require.NoError(t, s.SetHostHold(ctx, "node-1", true, "staff"))
			_, _, who = by()
			assert.Empty(t, who, "SetHostHold is not a team's")

			// Lifted: nothing is remembered, whatever the writer says.
			commit(HostHold{Host: "node-1", Wanted: false, By: fleet.HoldByTeam})
			wanted, _, who = by()
			assert.False(t, wanted)
			assert.Empty(t, who)
		})
	}
}
