package operation_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestRebootSurvivesTheAgentRestart(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := agenttest.NewEnv(t, sc)
	// The reboot is accepted but the "host" stays up, so that the test can
	// end the agent process the way a real reboot would.
	e.Host.SetInertReboot(true)
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	stop := e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.RebootNeedID, protocol.ImageNeedID("geth", agenttest.GethV2))
	e.Ctrl.Grant(e.Lease("lease-reboot", needs...))

	req := e.Ctrl.Await("phase rebooting", func(req *protocol.SyncRequest) bool {
		return req.Operation != nil && req.Operation.Phase == protocol.PhaseRebooting
	})
	assert.Equal(t, protocol.StepRunning, agenttest.StepOf(t, *req.Operation, protocol.RebootNeedID).Status)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, *req.Operation, needs[1].ID).Status)
	assert.True(t, req.Operation.Disrupted)

	// The process dies with the host ...
	stop()

	assert.Equal(t, "rebooting", agenttest.Field(t, e.StateOnDisk(), "current", "operation", "phase"),
		"the phase is on disk before the reboot")
	assert.Equal(t, "boot", agenttest.Field(t, e.StateOnDisk(), "current", "bootId"))

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.NotEqual(t, protocol.StateRunning, e.Host.Container(name).State, name+" is drained")
	}

	callsBefore := len(e.Host.Calls())

	// ... the host boots ...
	e.Host.SetInertReboot(false)
	require.NoError(t, e.Host.Reboot(context.Background()))
	require.Equal(t, "boot-1", e.Host.BootID())

	// ... and a new agent process finds the operation in its state file.
	e.Start()

	op := e.Ctrl.AwaitFinished("lease-reboot")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, protocol.RebootNeedID).Status)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, needs[1].ID).Status)

	// After the boot the agent only starts containers, EL first. It does
	// not reboot again (the one "reboot" here is the test's own).
	assert.Equal(t, []string{"reboot", "start geth", "start teku", "start validator"},
		e.Host.Calls()[callsBefore:])

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.Equal(t, protocol.StateRunning, e.Host.Container(name).State, name)
	}

	assert.Equal(t, agenttest.GethV2, e.Host.Container("geth").Digest)

	req = e.Ctrl.Await("all served", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Equal(t, "boot-1", req.BootID)
}

func TestRebootThatNeverHappensFails(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := agenttest.NewEnv(t, sc)
	e.Host.SetInertReboot(true)
	stop := e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.RebootNeedID)
	e.Ctrl.Grant(e.Lease("lease-reboot", needs...))

	e.Ctrl.Await("phase rebooting", func(req *protocol.SyncRequest) bool {
		return req.Operation != nil && req.Operation.Phase == protocol.PhaseRebooting
	})

	// Shortly after the reboot was issued an unchanged boot id is normal:
	// the agent keeps waiting.
	req := e.Ctrl.AwaitSyncs(5)
	require.Equal(t, protocol.PhaseRebooting, req.Operation.Phase)

	stop()

	callsBefore := len(e.Host.Calls())

	// Ten minutes later the agent runs again and the boot id still is the
	// same: the host did not reboot.
	e.ClockOffset.Store(int64(10 * time.Minute))
	e.Start()

	op := e.Ctrl.AwaitFinished("lease-reboot")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Contains(t, op.Error, "did not reboot")
	assert.Contains(t, op.Error, "boot id unchanged")
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepFailed, agenttest.StepOf(t, op, protocol.RebootNeedID).Status)

	// The drained containers are brought back all the same, and there is
	// no second reboot attempt.
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, e.Host.Calls()[callsBefore:])
	assert.Equal(t, "boot", e.Host.BootID())

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.Equal(t, protocol.StateRunning, e.Host.Container(name).State, name)
	}
}
