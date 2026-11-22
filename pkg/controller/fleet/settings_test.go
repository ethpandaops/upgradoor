package fleet

import (
	"encoding/json"
	"math"
	"testing"
	"time"

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
	assert.Equal(t, uint64(2), s.ReadyAttestations)
	assert.Equal(t, time.Minute, s.ReadyStableFor.Std())
	assert.Equal(t, 3, s.MinPeers)

	for _, v := range []uint64{0, MaxReadyAttestations + 1} {
		s.ReadyAttestations = v
		require.EqualError(t, s.Validate(), "readyAttestations must be between 1 and 32", "%d", v)
	}

	s.ReadyAttestations = MaxReadyAttestations
	require.NoError(t, s.Validate())

	s.ReadyStableFor = 0
	require.NoError(t, s.Validate(), "0: the first healthy sync suffices")

	for _, v := range []time.Duration{-time.Second, MaxReadyStableFor + time.Second} {
		s.ReadyStableFor = Duration(v)
		require.EqualError(t, s.Validate(), "readyStableFor must be between 0 and 30m", "%s", v)
	}

	s.ReadyStableFor = Duration(MaxReadyStableFor)
	require.NoError(t, s.Validate())

	s.MinPeers = 0
	require.EqualError(t, s.Validate(), "minPeers must be at least 1")

	s.MinPeers = 1
	require.NoError(t, s.Validate())

	assert.InDelta(t, 10.0, s.MaxLeasedPercent, 0)

	for _, v := range []float64{0, -1, 100.5, math.NaN()} {
		s.MaxLeasedPercent = v
		require.EqualError(t, s.Validate(), "maxLeasedPercent must be above 0 and at most 100", "%v", v)
	}

	s.MaxLeasedPercent = 100
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

func TestSettingsClientPolicies(t *testing.T) {
	s := DefaultSettings()
	assert.Equal(t, PolicyAuto, s.ImagePolicy("geth"), "a client nobody set follows policyImage")

	s.PolicyImage = PolicyManual
	s.PolicyClients = ClientPolicies{"geth": PolicyAuto}
	require.NoError(t, s.Validate())
	assert.Equal(t, PolicyAuto, s.ImagePolicy("geth"))
	assert.Equal(t, PolicyManual, s.ImagePolicy("lighthouse"))
	assert.Equal(t, PolicyManual, s.ImagePolicy(""), "a container without a client")

	raw, err := json.Marshal(DefaultSettings())
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"policyClients":{}`, "none is an object, never null")

	var back Settings
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Nil(t, back.PolicyClients)

	empty, same := DefaultSettings(), DefaultSettings()
	empty.PolicyClients = ClientPolicies{}
	assert.True(t, empty.Equal(&same))
	assert.False(t, s.Equal(&same))

	s.PolicyClients = ClientPolicies{"Geth": PolicyAuto}
	require.ErrorContains(t, s.Validate(), "is not a client name")

	s.PolicyClients = ClientPolicies{"geth": "sometimes"}
	require.ErrorContains(t, s.Validate(), "invalid policy")
}

func TestSettingsActionPolicies(t *testing.T) {
	s := DefaultSettings()
	assert.Equal(t, PolicyManual, s.ActionPolicy("security-upgrades"), "manual unless listed")

	raw, err := json.Marshal(s)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"actionPolicies":{}`)

	require.NoError(t, json.Unmarshal([]byte(`{"actionPolicies":{"security-upgrades":"auto"}}`), &s))
	assert.Equal(t, PolicyAuto, s.ActionPolicy("security-upgrades"))
	require.NoError(t, s.Validate())

	other := DefaultSettings()
	assert.False(t, s.Equal(&other))

	require.NoError(t, json.Unmarshal([]byte(`{"actionPolicies":{}}`), &s))
	assert.Nil(t, s.ActionPolicies, "an empty object is none")
	assert.True(t, s.Equal(&other))

	s.ActionPolicies = ActionPolicies{"Bad Name": PolicyAuto}
	require.ErrorContains(t, s.Validate(), "is not an action name")

	s.ActionPolicies = ActionPolicies{"a": "sometimes"}
	require.ErrorContains(t, s.Validate(), `invalid policy "sometimes" for a`)
}
