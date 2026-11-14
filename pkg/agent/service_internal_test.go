package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// inventorySystem is a System of which only Inventory is used.
type inventorySystem struct {
	inventory func(ctx context.Context) (*protocol.Inventory, error)
}

func (s *inventorySystem) Status(context.Context) (*SystemStatus, error) {
	return nil, errors.New("not used")
}

func (s *inventorySystem) Inventory(ctx context.Context) (*protocol.Inventory, error) {
	return s.inventory(ctx)
}

func (s *inventorySystem) UpgradePackages(context.Context, []string) error {
	return errors.New("not used")
}

func (s *inventorySystem) Reboot(context.Context) error { return errors.New("not used") }

// A package query that hangs must not stall the sync loop.
func TestInventoryCollectionIsBounded(t *testing.T) {
	log, logs := test.NewNullLogger()

	svc := &service{
		log: log,
		deps: Deps{System: &inventorySystem{
			inventory: func(ctx context.Context) (*protocol.Inventory, error) {
				<-ctx.Done()

				return nil, ctx.Err()
			},
		}},
		inventoryTimeout: 20 * time.Millisecond,
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		svc.sendInventory(t.Context())
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "sendInventory waits for a hanging inventory without a bound")
	}

	entry := logs.LastEntry()
	require.NotNil(t, entry)
	assert.Equal(t, "cannot collect the inventory", entry.Message)

	logged, ok := entry.Data[logrus.ErrorKey].(error)
	require.True(t, ok, "the error is logged")
	assert.ErrorIs(t, logged, context.DeadlineExceeded)
}

func TestMissingInventoryIsNotLoggedAsANilError(t *testing.T) {
	log, logs := test.NewNullLogger()

	svc := &service{
		log: log,
		deps: Deps{System: &inventorySystem{
			//nolint:nilnil // exactly the answer under test
			inventory: func(context.Context) (*protocol.Inventory, error) { return nil, nil },
		}},
		inventoryTimeout: time.Second,
	}

	// No client is set: nothing may be uploaded.
	svc.sendInventory(t.Context())

	entry := logs.LastEntry()
	require.NotNil(t, entry)
	assert.Contains(t, entry.Message, "no inventory")
	assert.NotContains(t, entry.Data, logrus.ErrorKey)
}
