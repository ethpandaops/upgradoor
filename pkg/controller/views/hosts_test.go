package views

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

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
