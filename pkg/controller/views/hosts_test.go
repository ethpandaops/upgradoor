package views

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// A host's validator count is the plan's count of its active validators;
// the counted range only before the first plan.
func TestSummarizeCountsActiveValidators(t *testing.T) {
	host := &fleet.Host{Name: "a", Validators: []identity.ValidatorRange{{Start: 0, End: 100}}}
	now := time.Unix(1800000000, 0)

	assert.Equal(t, uint64(100), Summarize(host, nil, now, time.Minute).Validators)

	plan := &scheduler.Plan{Hosts: map[string]scheduler.HostPlan{"a": {Weight: 40}}}
	assert.Equal(t, uint64(40), Summarize(host, plan, now, time.Minute).Validators)
}

// The host row carries the agent's protocol version and, for one the
// controller does not serve, why.
func TestSummarizeAgentProtocol(t *testing.T) {
	now := time.Unix(1800000000, 0)
	host := &fleet.Host{Name: "a", Status: &protocol.SyncRequest{ProtocolVersion: protocol.Version}}

	sum := Summarize(host, nil, now, time.Minute)
	assert.Equal(t, protocol.Version, sum.AgentProtocol)
	assert.Empty(t, sum.Incompatible)

	host.Status.ProtocolVersion = protocol.Version + 1
	sum = Summarize(host, nil, now, time.Minute)
	listing := Listing(&sum)
	assert.Equal(t, protocol.Version+1, listing.AgentProtocol)
	assert.Equal(t, fleet.Unsupported(protocol.Version+1), listing.Incompatible)
	assert.NotEmpty(t, listing.Incompatible)
}

func TestNewHostUsage(t *testing.T) {
	busy := 37.46
	res := &protocol.Resources{
		CPU:    protocol.CPUStats{Busy: &busy},
		Memory: protocol.MemoryStats{Total: 1000, Available: 250},
	}

	got := NewHostUsage(res)
	assert.Equal(t, 37, *got.CPU)
	assert.Equal(t, 75, *got.Memory)

	assert.Nil(t, NewHostUsage(nil))
	assert.Nil(t, NewHostUsage(&protocol.Resources{}), "nothing known")
	assert.Equal(t, &HostUsage{Memory: got.Memory}, NewHostUsage(&protocol.Resources{Memory: res.Memory}),
		"no busy share before the agent's second reading")
}

// An image rollout names the setting its policy comes from; other rollouts
// name none.
func TestRolloutsPolicySource(t *testing.T) {
	st, sched := controllertest.NewStore(), controllertest.NewScheduler()
	sched.CurrentSettings.PolicyClients = fleet.ClientPolicies{"geth": fleet.PolicyManual}
	sched.CurrentSettings.PolicyKinds = fleet.KindPolicies{"cl": fleet.PolicyManual}

	st.Rollouts = []*fleet.Rollout{
		{ID: "geth", Kind: fleet.RolloutGroup, ContainerKind: protocol.KindEL, Team: "geth", State: fleet.RolloutActive},
		{ID: "teku", Kind: fleet.RolloutGroup, ContainerKind: protocol.KindCL, Team: "teku", State: fleet.RolloutActive},
		{ID: "tools", Kind: fleet.RolloutGroup, State: fleet.RolloutActive},
		{ID: "reboot", Kind: fleet.RolloutReboot, State: fleet.RolloutActive},
	}

	v := New(Options{Store: st, Scheduler: sched, Now: time.Now})

	out, err := v.Rollouts(context.Background(), 10)
	require.NoError(t, err)

	got := make(map[string]fleet.PolicySource, len(out.Open))
	for _, r := range out.Open {
		got[r.ID] = r.PolicySource
	}

	assert.Equal(t, map[string]fleet.PolicySource{
		"geth": fleet.PolicySourceClient, "teku": fleet.PolicySourceKind,
		"tools": fleet.PolicySourceDefault, "reboot": "",
	}, got)
}
