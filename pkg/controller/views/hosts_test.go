package views

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/identity"
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
