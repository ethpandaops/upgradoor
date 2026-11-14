package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

func TestLiveness(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			empty, err := s.ListLiveness(ctx, 5)
			require.NoError(t, err)
			assert.NotNil(t, empty)
			assert.Empty(t, empty)

			require.NoError(t, s.SaveLiveness(ctx, nil), "no rows is a no-op")
			require.NoError(t, s.PruneLiveness(ctx, 100), "pruning nothing is fine")

			// Epochs 10, 11, 13 (12 is missing: the beacon node was down);
			// node-b has no row for 13. Hosts need not be registered.
			require.NoError(t, s.SaveLiveness(ctx, []fleet.Liveness{
				{Host: "node-b", Epoch: 10, Live: 90, Total: 100},
				{Host: "node-a", Epoch: 10, Live: 100, Total: 100},
				{Host: "node-a", Epoch: 11, Live: 99, Total: 100},
				{Host: "node-b", Epoch: 11, Live: 0, Total: 100},
				{Host: "node-a", Epoch: 13, Live: 100, Total: 100},
			}))

			tests := []struct {
				name   string
				epochs int
				want   []fleet.Liveness
			}{
				{"zero epochs", 0, []fleet.Liveness{}},
				{"negative epochs", -1, []fleet.Liveness{}},
				{"newest epoch", 1, []fleet.Liveness{
					{Host: "node-a", Epoch: 13, Live: 100, Total: 100},
				}},
				{"two newest epochs that have rows", 2, []fleet.Liveness{
					{Host: "node-a", Epoch: 13, Live: 100, Total: 100},
					{Host: "node-a", Epoch: 11, Live: 99, Total: 100},
					{Host: "node-b", Epoch: 11, Live: 0, Total: 100},
				}},
				{"more than exist", 50, []fleet.Liveness{
					{Host: "node-a", Epoch: 13, Live: 100, Total: 100},
					{Host: "node-a", Epoch: 11, Live: 99, Total: 100},
					{Host: "node-b", Epoch: 11, Live: 0, Total: 100},
					{Host: "node-a", Epoch: 10, Live: 100, Total: 100},
					{Host: "node-b", Epoch: 10, Live: 90, Total: 100},
				}},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					got, getErr := s.ListLiveness(ctx, tt.epochs)
					require.NoError(t, getErr)
					assert.Equal(t, tt.want, got)
				})
			}

			// Upsert: a row for the same host and epoch is replaced, also
			// when it appears twice in one call (the last one wins).
			require.NoError(t, s.SaveLiveness(ctx, []fleet.Liveness{
				{Host: "node-b", Epoch: 11, Live: 50, Total: 100},
				{Host: "node-b", Epoch: 11, Live: 97, Total: 101},
				{Host: "node-b", Epoch: 13, Live: 100, Total: 101},
			}))

			got, err := s.ListLiveness(ctx, 2)
			require.NoError(t, err)
			assert.Equal(t, []fleet.Liveness{
				{Host: "node-a", Epoch: 13, Live: 100, Total: 100},
				{Host: "node-b", Epoch: 13, Live: 100, Total: 101},
				{Host: "node-a", Epoch: 11, Live: 99, Total: 100},
				{Host: "node-b", Epoch: 11, Live: 97, Total: 101},
			}, got)
			assert.Equal(t, 6, countRows(t, s, "validator_liveness"))

			// Prune: strictly older than the given epoch.
			require.NoError(t, s.PruneLiveness(ctx, 11))

			got, err = s.ListLiveness(ctx, 50)
			require.NoError(t, err)
			assert.Len(t, got, 4)

			for _, row := range got {
				assert.GreaterOrEqual(t, row.Epoch, uint64(11))
			}

			require.NoError(t, s.PruneLiveness(ctx, 0), "epoch 0 prunes nothing")
			assert.Equal(t, 4, countRows(t, s, "validator_liveness"))

			require.NoError(t, s.PruneLiveness(ctx, 14))
			assert.Zero(t, countRows(t, s, "validator_liveness"))
		})
	}
}
