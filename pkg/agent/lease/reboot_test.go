package lease

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

	e := newEnv(t, sc)
	// The host stays up after the reboot call; the test stops the runner instead.
	e.host.SetInertReboot(true)
	e.pull(agenttest.GethImage, agenttest.GethV2)

	r := e.start(t.Context())
	geth := gethNeed(agenttest.GethV2)
	e.grant("lease-reboot", agenttest.RebootNeed(), geth)

	e.awaitPhase("lease-reboot", protocol.PhaseRebooting)

	op := r.Current().Operation
	assert.Equal(t, protocol.StepRunning, stepOf(t, &op, protocol.RebootNeedID).Status)
	assert.Equal(t, protocol.StepDone, stepOf(t, &op, geth.ID).Status)
	assert.True(t, op.Disrupted)

	require.NoError(t, r.Stop())

	assert.Equal(t, "rebooting", field(t, e.onDisk(), "current", "operation", "phase"),
		"the phase is on disk before the reboot")
	assert.Equal(t, "boot", field(t, e.onDisk(), "current", "bootId"))

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.NotEqual(t, protocol.StateRunning, e.host.Container(name).State, name+" is drained")
	}

	callsBefore := len(e.host.Calls())

	// The host boots, and a new runner finds the operation in the state file.
	e.host.SetInertReboot(false)
	require.NoError(t, e.host.Reboot(context.Background()))
	require.Equal(t, "boot-1", e.host.BootID())

	e.start(t.Context())

	done := e.awaitFinished("lease-reboot")
	require.Equal(t, protocol.PhaseDone, done.Phase, done.Error)
	assert.True(t, done.Disrupted)
	assert.Equal(t, protocol.StepDone, stepOf(t, done, protocol.RebootNeedID).Status)
	assert.Equal(t, protocol.StepDone, stepOf(t, done, geth.ID).Status)

	// After the boot only starts, EL first; the one "reboot" is the test's.
	assert.Equal(t, []string{"reboot", "start geth", "start teku", "start validator"},
		e.host.Calls()[callsBefore:])
	e.assertAllRunning()
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest)
}

func TestRebootThatNeverHappensFails(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.host.SetInertReboot(true)

	r := e.start(t.Context())
	e.grant("lease-reboot", agenttest.RebootNeed())

	e.awaitPhase("lease-reboot", protocol.PhaseRebooting)

	// Shortly after the reboot was issued an unchanged boot id is normal.
	time.Sleep(10 * time.Millisecond)
	require.Equal(t, protocol.PhaseRebooting, r.Current().Operation.Phase)
	require.NoError(t, r.Stop())

	callsBefore := len(e.host.Calls())

	// Ten minutes later the boot id still is the same.
	e.offset.Store(int64(10 * time.Minute))
	e.start(t.Context())

	op := e.awaitFinished("lease-reboot")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Contains(t, op.Error, "did not reboot")
	assert.Contains(t, op.Error, "boot id unchanged")
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepFailed, stepOf(t, op, protocol.RebootNeedID).Status)

	// The drained containers come back, and there is no second reboot.
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, e.host.Calls()[callsBefore:])
	assert.Equal(t, "boot", e.host.BootID())
	e.assertAllRunning()
}
