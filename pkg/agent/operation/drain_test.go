package operation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestRebootOrder(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()
	sc.RebootRequired = true
	sc.NewestKernel = "6.12.48-amd64"

	e := agenttest.NewEnv(t, sc)
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")
	e.Start()

	needs := agenttest.AwaitNeeds(t, e,
		protocol.ImageNeedID("teku", agenttest.TekuV2),
		protocol.RebootNeedID,
		protocol.ImageNeedID("geth", agenttest.GethV2),
		agenttest.PackagesNeed().ID,
	)
	e.Ctrl.Grant(e.Lease("lease-reboot", needs...))

	op := e.Ctrl.AwaitFinished("lease-reboot")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepDone, s.Status, s.NeedID)
	}

	// One downtime: drain VC, CL, EL; upgrade; recreate without starting;
	// reboot; start EL, CL, VC. The unmanaged container is left alone.
	assert.Equal(t, []string{
		"stop validator", "stop teku", "stop geth",
		"upgrade docker-ce",
		"recreate geth " + agenttest.GethV2 + " start=false",
		"recreate teku " + agenttest.TekuV2 + " start=false",
		"reboot",
		"start geth", "start teku", "start validator",
	}, agenttest.Disruptive(e.Host.Calls()))

	assert.Equal(t, "boot-1", e.Host.BootID())

	for _, name := range []string{"geth", "teku", "validator", "dora"} {
		assert.Equal(t, protocol.StateRunning, e.Host.Container(name).State, name)
	}

	req := e.Ctrl.Await("all served", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Equal(t, "boot-1", req.BootID)
	assert.False(t, req.OS.RebootRequired)

	assertPhaseOrder(t, e, "lease-reboot", protocol.PhaseDone)
}
