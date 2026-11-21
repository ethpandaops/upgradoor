package scheduler

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestNeedImpact(t *testing.T) {
	h := &fleet.Host{Status: &protocol.SyncRequest{Containers: []protocol.Container{
		{Name: "exporter", Impact: protocol.ImpactIndependent},
		{Name: "mev", Impact: protocol.ImpactSlot},
		{Name: "old"},
	}}}

	exporter := imageNeed("exporter", "org/exporter:1", digestA)
	mev := imageNeed("mev", "org/mev:1", digestA)

	assert.Equal(t, protocol.ImpactIndependent, needImpact(h, &exporter))
	assert.Equal(t, protocol.ImpactSlot, needImpact(h, &mev))

	for name, n := range map[string]protocol.Need{
		"no class reported":  imageNeed("old", "org/old:1", digestA),
		"unknown container":  imageNeed("gone", "org/gone:1", digestA),
		"about no container": rebootNeed(),
	} {
		assert.Equal(t, protocol.ImpactNode, needImpact(h, &n), name)
	}

	assert.Equal(t, protocol.ImpactSlot, needsImpact(h, []protocol.Need{exporter, mev}))
	assert.Equal(t, protocol.ImpactNode, needsImpact(h, []protocol.Need{exporter, packagesNeed("2")}))
	assert.Equal(t, protocol.ImpactNode, needImpact(&fleet.Host{}, &exporter), "a host that never synced")
}

func TestOccupation(t *testing.T) {
	assert.Empty(t, (&hostView{h: &fleet.Host{}}).occupation())
	assert.Equal(t, protocol.ImpactNode, (&hostView{lease: &fleet.Lease{}}).occupation(), "a lease without a class")
	assert.Equal(t, protocol.ImpactIndependent,
		(&hostView{lease: &fleet.Lease{Impact: protocol.ImpactIndependent}}).occupation())

	working := &hostView{h: &fleet.Host{Status: &protocol.SyncRequest{
		Operation: &protocol.Operation{Phase: protocol.PhaseApplying},
	}}}
	assert.Equal(t, protocol.ImpactNode, working.occupation(), "at work without a lease")
}
