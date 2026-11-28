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
	require.EqualError(t, s.Validate(), "baselineGrace must be between readyStableFor and readinessTimeout",
		"the grace is shorter than the run of healthy syncs")

	s.BaselineGrace = s.ReadyStableFor
	require.NoError(t, s.Validate())

	s.BaselineGrace = s.ReadinessTimeout + 1
	require.EqualError(t, s.Validate(), "baselineGrace must be between readyStableFor and readinessTimeout")

	s.BaselineGrace = s.ReadinessTimeout
	require.NoError(t, s.Validate())

	assert.Equal(t, 15*time.Minute, DefaultSettings().BaselineGrace.Std())
	assert.Equal(t, 10, DefaultSettings().MaxConcurrentPulls)

	for _, v := range []int{-1, MaxConcurrentPulls + 1} {
		s.MaxConcurrentPulls = v
		require.EqualError(t, s.Validate(), "maxConcurrentPulls must be between 0 and 10000", "%d", v)
	}

	s.MaxConcurrentPulls = 0
	require.NoError(t, s.Validate(), "0: no bound")

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

func TestDeployWindowState(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	armed := start.Add(time.Hour)

	for _, tt := range []struct {
		name   string
		w      DeployWindowState
		length time.Duration
		now    time.Time
		open   bool
	}{
		{name: "no window stored", w: DeployWindowState{Length: 24 * time.Hour}, now: start},
		{name: "open", w: DeployWindowState{StartedAt: start, Length: 24 * time.Hour},
			now: start.Add(23 * time.Hour), open: true},
		{name: "ended by time", w: DeployWindowState{StartedAt: start, Length: 24 * time.Hour},
			now: start.Add(24 * time.Hour)},
		{name: "longer window moves the end", w: DeployWindowState{StartedAt: start, Length: 48 * time.Hour},
			now: start.Add(24 * time.Hour), open: true},
		{name: "disabled", w: DeployWindowState{StartedAt: start}, now: start},
		{name: "armed", w: DeployWindowState{StartedAt: start, Length: 24 * time.Hour, ArmedAt: &armed},
			now: start.Add(2 * time.Hour)},
	} {
		assert.Equal(t, tt.open, tt.w.Open(tt.now), tt.name)
	}

	w := DeployWindowState{StartedAt: start, Length: time.Hour}
	assert.Equal(t, start.Add(time.Hour), w.End())

	w.Length = 0
	assert.True(t, w.End().IsZero())
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
	p, src := s.ImagePolicy("geth", protocol.KindEL)
	assert.Equal(t, PolicyAuto, p, "a client nobody set follows policyImage")
	assert.Equal(t, PolicySourceDefault, src)

	s.PolicyImage = PolicyManual
	s.PolicyClients = ClientPolicies{"el": {"geth": PolicyAuto}, "cl": {"lighthouse": PolicyAuto}}
	require.NoError(t, s.Validate())

	p, src = s.ImagePolicy("geth", protocol.KindEL)
	assert.Equal(t, PolicyAuto, p)
	assert.Equal(t, PolicySourceClient, src)

	p, src = s.ImagePolicy("lighthouse", protocol.KindVC)
	assert.Equal(t, PolicyManual, p, "a team's policy is per kind")
	assert.Equal(t, PolicySourceDefault, src)

	p, _ = s.ImagePolicy("lighthouse", protocol.KindCL)
	assert.Equal(t, PolicyAuto, p)
	s.PolicyClients = s.PolicyClients.With(protocol.KindCL, "lighthouse", "")

	p, _ = s.ImagePolicy("lighthouse", protocol.KindCL)
	assert.Equal(t, PolicyManual, p)

	p, _ = s.ImagePolicy("", "")
	assert.Equal(t, PolicyManual, p, "a container without a client")

	raw, err := json.Marshal(DefaultSettings())
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"policyClients":{}`, "none is an object, never null")

	var back Settings
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Nil(t, back.PolicyClients)

	empty, same := DefaultSettings(), DefaultSettings()
	empty.PolicyClients = ClientPolicies{"el": {}}
	assert.True(t, empty.Equal(&same))
	assert.False(t, s.Equal(&same))

	require.NoError(t, json.Unmarshal([]byte(`{"policyClients":{"el":{},"cl":{"teku":"manual"}}}`), &back))
	assert.Equal(t, ClientPolicies{"cl": {"teku": PolicyManual}}, back.PolicyClients, "a kind without entries is none")

	s.PolicyClients = ClientPolicies{"el": {"Geth": PolicyAuto}}
	require.ErrorContains(t, s.Validate(), "is not a client name")

	s.PolicyClients = ClientPolicies{"EL": {"geth": PolicyAuto}}
	require.ErrorContains(t, s.Validate(), "is not a kind name")

	s.PolicyClients = ClientPolicies{"el": {"geth": "sometimes"}}
	require.ErrorContains(t, s.Validate(), "invalid policy")
}

// An image rollout of Ansible deploys only is auto whatever the image
// policies say; one with a tag that moved answers to them.
func TestSettingsGroupPolicy(t *testing.T) {
	s := DefaultSettings()
	s.PolicyClients = ClientPolicies{"el": {"geth": PolicyManual}}

	deploy := RolloutTarget{Image: "geth:v1", Digest: "sha256:a", Spec: "k1"}
	moved := RolloutTarget{Image: "geth:v1", Digest: "sha256:b"}

	for _, tt := range []struct {
		name    string
		targets []RolloutTarget
		policy  Policy
		source  PolicySource
	}{
		{"all deploys: auto", []RolloutTarget{deploy, deploy}, PolicyAuto, PolicySourceDeploy},
		{"a deploy and a moved tag: the image policy", []RolloutTarget{deploy, moved}, PolicyManual, PolicySourceClient},
		{"moved tags only: the image policy", []RolloutTarget{moved}, PolicyManual, PolicySourceClient},
		{"no targets: the image policy", nil, PolicyManual, PolicySourceClient},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &Rollout{Kind: RolloutGroup, ContainerKind: protocol.KindEL, Team: "geth", Targets: tt.targets}
			p, src := s.GroupPolicy(r)
			assert.Equal(t, tt.policy, p)
			assert.Equal(t, tt.source, src)
			assert.Equal(t, tt.policy, s.RolloutPolicy(r))
		})
	}
}

func TestSettingsKindPolicies(t *testing.T) {
	s := DefaultSettings()
	assert.Nil(t, s.PolicyKinds, "none by default")

	s.PolicyClients = ClientPolicies{"el": {"geth": PolicyAuto}}
	s.PolicyKinds = KindPolicies{"el": PolicyManual, "cl": PolicyManual, "snooper-el": PolicyManual, KindOther: PolicyManual}
	require.NoError(t, s.Validate())

	for _, tt := range []struct {
		name   string
		team   string
		kind   protocol.ContainerKind
		policy Policy
		source PolicySource
	}{
		{"the client's policy before the kind's", "geth", protocol.KindEL, PolicyAuto, PolicySourceClient},
		{"a client without one: the kind's", "besu", protocol.KindEL, PolicyManual, PolicySourceKind},
		{"no team: the kind's", "", "snooper-el", PolicyManual, PolicySourceKind},
		{"unlabelled: other", "", "", PolicyManual, PolicySourceKind},
		{"a kind not listed: policyImage", "lighthouse", protocol.KindVC, PolicyAuto, PolicySourceDefault},
		{"an unknown kind: policyImage", "", "custom", PolicyAuto, PolicySourceDefault},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, src := s.ImagePolicy(tt.team, tt.kind)
			assert.Equal(t, tt.policy, p)
			assert.Equal(t, tt.source, src)
		})
	}

	group := &Rollout{Kind: RolloutGroup, ContainerKind: protocol.KindCL, Team: "lighthouse"}
	assert.Equal(t, PolicyManual, s.RolloutPolicy(group))

	pkgs := &Rollout{Kind: RolloutPackages}
	assert.Equal(t, PolicyAuto, s.RolloutPolicy(pkgs), "packages ignore the kind policies")

	raw, err := json.Marshal(DefaultSettings())
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"policyKinds":{}`, "none is an object, never null")

	var back Settings
	require.NoError(t, json.Unmarshal([]byte(`{"policyKinds":{}}`), &back))
	assert.Nil(t, back.PolicyKinds)

	empty, same := DefaultSettings(), DefaultSettings()
	empty.PolicyKinds = KindPolicies{}
	assert.True(t, empty.Equal(&same))
	assert.False(t, s.Equal(&same))

	for msg, kinds := range map[string]KindPolicies{
		"is not a kind name": {"EL": PolicyAuto},
		"\"none\" is not":    {"none": PolicyAuto},
		"invalid policy":     {"cl": "sometimes"},
	} {
		s.PolicyKinds = kinds
		require.ErrorContains(t, s.Validate(), msg)
	}
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

func TestClientPoliciesWith(t *testing.T) {
	var c ClientPolicies

	c = c.With(protocol.KindCL, "lighthouse", PolicyManual)
	d := c.With("", "tools", PolicyAuto)
	assert.Equal(t, ClientPolicies{"cl": {"lighthouse": PolicyManual}}, c, "With copies")
	assert.Equal(t, ClientPolicies{"cl": {"lighthouse": PolicyManual}, KindOther: {"tools": PolicyAuto}}, d)
	assert.Equal(t, 2, d.Len())

	p, ok := d.Get("", "tools")
	assert.True(t, ok)
	assert.Equal(t, PolicyAuto, p)

	_, ok = d.Get(protocol.KindVC, "lighthouse")
	assert.False(t, ok)

	assert.Nil(t, c.With(protocol.KindCL, "lighthouse", ""), "the last entry cleared is none")
	assert.True(t, d.Equal(d.Clone()))
	assert.False(t, d.Equal(c))
}
