package fleet

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestEffectiveMinPeers(t *testing.T) {
	for _, tt := range []struct {
		minPeers, nodes, want int
	}{
		{minPeers: 3, nodes: 0, want: 1},
		{minPeers: 3, nodes: 1, want: 1},
		{minPeers: 3, nodes: 2, want: 1},
		{minPeers: 3, nodes: 3, want: 1},
		{minPeers: 3, nodes: 4, want: 2},
		{minPeers: 3, nodes: 5, want: 2},
		{minPeers: 3, nodes: 6, want: 3},
		{minPeers: 3, nodes: 7, want: 3},
		{minPeers: 3, nodes: 100, want: 3},
		{minPeers: 1, nodes: 100, want: 1},
		{minPeers: 25, nodes: 10, want: 5},
		{minPeers: 25, nodes: 11, want: 5},
	} {
		s := Settings{MinPeers: tt.minPeers}
		assert.Equal(t, tt.want, s.EffectiveMinPeers(tt.nodes), "minPeers %d, %d nodes", tt.minPeers, tt.nodes)
	}
}

func TestSettingsDefaultsAndValidation(t *testing.T) {
	s := DefaultSettings()
	require.NoError(t, s.Validate())
	assert.Equal(t, uint64(1), s.ReadyEpochs)
	assert.Equal(t, 3, s.MinPeers)

	s.ReadyEpochs = 0
	require.NoError(t, s.Validate(), "0: no waiting beyond one healthy sync")

	s.MinPeers = 0
	require.EqualError(t, s.Validate(), "minPeers must be at least 1")

	s.MinPeers = 1
	require.NoError(t, s.Validate())
}

func TestSettingsCanaryPauseAndPeerShare(t *testing.T) {
	s := DefaultSettings()
	assert.Zero(t, s.ReadyPeerShare, "the comparison with the fleet is off by default")

	for _, kind := range []protocol.NeedKind{protocol.NeedImage, protocol.NeedPackages, protocol.NeedReboot} {
		assert.False(t, s.PauseAfterCanaryFor(kind), "%s: no pause after the canary by default", kind)
	}

	s.PauseAfterCanaryPackages = true
	assert.True(t, s.PauseAfterCanaryFor(protocol.NeedPackages))
	assert.False(t, s.PauseAfterCanaryFor(protocol.NeedImage))
	assert.False(t, s.PauseAfterCanaryFor(protocol.NeedReboot))
	assert.False(t, s.PauseAfterCanaryFor(""))

	for _, share := range []float64{0, 0.5, 1} {
		s.ReadyPeerShare = share
		require.NoError(t, s.Validate(), "%v", share)
	}

	for _, share := range []float64{-0.1, 1.01, math.NaN()} {
		s.ReadyPeerShare = share
		require.EqualError(t, s.Validate(), "readyPeerShare must be between 0 and 1", "%v", share)
	}
}
