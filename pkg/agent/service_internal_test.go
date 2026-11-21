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

	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// inventorySystem is a hostapi.System of which only Inventory is used.
type inventorySystem struct {
	inventory func(ctx context.Context) (*protocol.Inventory, error)
}

func (s *inventorySystem) Status(context.Context) (*hostapi.SystemStatus, error) {
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
	t.Parallel()

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

func TestLeaseDeadline(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		expires time.Time
		ttl     int64
		want    time.Time
	}{
		{name: "clocks agree", expires: now.Add(30 * time.Minute), ttl: 1800, want: now.Add(30 * time.Minute)},
		{name: "controller clock ahead", expires: now.Add(2 * time.Hour), ttl: 1800, want: now.Add(30 * time.Minute)},
		{name: "controller clock behind", expires: now.Add(-time.Hour), ttl: 1800, want: now.Add(-time.Hour)},
		{name: "restated late", expires: now.Add(10 * time.Minute), ttl: 600, want: now.Add(10 * time.Minute)},
		{name: "no ttl", expires: now.Add(time.Hour), ttl: 0, want: now},
		{name: "negative ttl", expires: now.Add(time.Hour), ttl: -5, want: now},
		{name: "no expiry", ttl: 60, want: now.Add(time.Minute)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := leaseDeadline(&protocol.Lease{ExpiresAt: tt.expires, TTLSeconds: tt.ttl}, now)
			assert.Equal(t, tt.want, got)
		})
	}
}
