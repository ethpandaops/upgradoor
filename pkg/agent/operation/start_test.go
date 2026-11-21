package operation_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestFailedStepStillRestartsContainers(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")
	e.Host.SetRegistry(agenttest.VCImage, agenttest.VCV2, "")
	e.Host.Fail("recreate:teku", errors.New("port is already allocated"))
	e.Start()

	needs := agenttest.AwaitNeeds(t, e,
		protocol.ImageNeedID("validator", agenttest.VCV2),
		protocol.ImageNeedID("teku", agenttest.TekuV2),
		protocol.ImageNeedID("geth", agenttest.GethV2),
	)
	e.Ctrl.Grant(e.Lease("lease-broken", needs...))

	op := e.Ctrl.AwaitFinished("lease-broken")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.True(t, op.Disrupted)
	assert.Contains(t, op.Error, "recreate container teku")
	assert.Contains(t, op.Error, "port is already allocated")

	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, needs[2].ID).Status, "geth came first and worked")

	teku := agenttest.StepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepFailed, teku.Status)
	assert.Contains(t, teku.Error, "port is already allocated")

	vc := agenttest.StepOf(t, op, needs[0].ID)
	assert.Equal(t, protocol.StepPending, vc.Status, "nothing is attempted after a failure")
	assert.Contains(t, vc.Error, "not attempted")

	// The broken recreate left teku stopped; the agent starts everything
	// that ran before, and does not roll anything back.
	assert.Equal(t, []string{
		"recreate geth " + agenttest.GethV2 + " start=true",
		"recreate teku " + agenttest.TekuV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, agenttest.Disruptive(e.Host.Calls()))

	assert.Equal(t, protocol.StateRunning, e.Host.Container("teku").State)
	assert.Equal(t, agenttest.GethV2, e.Host.Container("geth").Digest, "no rollback")
	assert.Equal(t, agenttest.VCV1, e.Host.Container("validator").Digest)

	assertPhaseOrder(t, e, "lease-broken", protocol.PhaseFailed)
}

// A container that crashed on a broken image must come back when a newer
// image is rolled onto it; one that somebody stopped must stay down.
func TestRollStartsACrashedContainerButNotAStoppedOne(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())

	// teku died by itself; the validator was stopped by a person.
	require.NoError(t, e.Host.Exit("teku", 1))
	require.NoError(t, e.Host.Exit("validator", 143))

	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")
	e.Host.SetRegistry(agenttest.VCImage, agenttest.VCV2, "")
	e.Start()

	needs := agenttest.AwaitNeeds(t, e,
		protocol.ImageNeedID("teku", agenttest.TekuV2),
		protocol.ImageNeedID("validator", agenttest.VCV2),
	)
	e.Ctrl.Grant(e.Lease("lease-repair", needs...))

	op := e.Ctrl.AwaitFinished("lease-repair")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	calls := agenttest.Disruptive(e.Host.Calls())
	assert.Contains(t, calls, "recreate teku "+agenttest.TekuV2+" start=true")
	assert.Contains(t, calls, "recreate validator "+agenttest.VCV2+" start=false")
	assert.NotContains(t, calls, "start validator")

	assert.Equal(t, protocol.StateRunning, e.Host.Container("teku").State)
	assert.Equal(t, "created", e.Host.Container("validator").State, "recreated, not started")
	assert.Equal(t, agenttest.VCV2, e.Host.Container("validator").Digest, "rolled all the same")
}
