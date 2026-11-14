package beacon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLiveBeaconNode runs one poll against a real beacon node. It only reads
// (a handful of GETs and liveness POSTs for 16 validators) and is skipped
// unless UPGRADOOR_BEACON_URL is set:
//
//	UPGRADOOR_BEACON_URL=https://beacon.example.org go test -run TestLiveBeaconNode -v ./pkg/controller/beacon/
func TestLiveBeaconNode(t *testing.T) {
	endpoint := os.Getenv("UPGRADOOR_BEACON_URL")
	if endpoint == "" {
		t.Skip("UPGRADOOR_BEACON_URL is not set")
	}

	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)

	store := newFakeStore(host("smoke-a", 0, 8), host("smoke-b", 8, 16), host("no-validators", 0, 0))

	svc, ok := New(log, Options{Endpoints: []string{endpoint}, Store: store}).(*service)
	require.True(t, ok)
	require.NoError(t, svc.init())

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	svc.poll(ctx)

	health := svc.Health()
	t.Logf("health: %+v", health)

	require.False(t, health.UpdatedAt.IsZero(), "poll failed: %s", health.Error)
	assert.False(t, health.GenesisTime.IsZero())
	assert.Positive(t, health.SecondsPerSlot)
	assert.Positive(t, health.SlotsPerEpoch)
	assert.Positive(t, health.HeadSlot)
	assert.GreaterOrEqual(t, health.CurrentEpoch, health.FinalizedEpoch)
	assert.Equal(t, health.CurrentEpoch-health.FinalizedEpoch, health.FinalityLagEpochs)

	// The wall-clock slot must agree with the head of a live chain; this is
	// what proves the slot-duration maths against a real spec.
	clockSlot := svc.timing.slotAt(time.Now())
	assert.InDelta(t, float64(clockSlot), float64(health.HeadSlot), float64(2*health.SlotsPerEpoch),
		"wall-clock slot %d is far from head slot %d", clockSlot, health.HeadSlot)

	if !health.LivenessKnown {
		t.Logf("liveness unknown on this node: %s", health.Error)

		return
	}

	assert.Empty(t, health.Error)
	assert.Equal(t, uint64(16), health.Tracked)
	assert.LessOrEqual(t, health.Live, health.Tracked)
	assert.GreaterOrEqual(t, health.LivenessEpoch+2, health.CurrentEpoch)

	for _, name := range []string{"smoke-a", "smoke-b"} {
		row, found := store.row(name, health.LivenessEpoch)
		require.True(t, found, name)
		assert.Equal(t, uint64(8), row.Total)
		t.Logf("%s: %d/%d live in epoch %d", name, row.Live, row.Total, row.Epoch)
	}
}
