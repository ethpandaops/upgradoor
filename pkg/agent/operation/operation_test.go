package operation_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// A process that is killed (or a host that loses power) in the middle of a
// step leaves the phase applying behind; a regular stop never does.
func TestKilledOperationIsResumedAsFailed(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")

	// The first agent process dies inside the recreate of geth: the call
	// never returns to it. Its sync loop and watcher are ended; the
	// operation goroutine stays parked in the call until the test is over,
	// which is as dead as a goroutine gets.
	ctx, kill := context.WithCancel(t.Context())
	defer kill()

	inCall, release := agenttest.BlockIn(e, "recreate geth")

	dead := e.NewAgent()
	require.NoError(t, dead.Start(ctx))

	t.Cleanup(func() {
		release()
		require.NoError(t, dead.Stop())
	})

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2), protocol.ImageNeedID("teku", agenttest.TekuV2))
	e.Ctrl.Grant(e.Lease("lease-cut", needs...))

	agenttest.AwaitParked(t, inCall, "recreate geth")
	kill()

	assert.Equal(t, "applying", agenttest.Field(t, e.StateOnDisk(), "current", "operation", "phase"))

	// Nobody knows what the cut-off step left behind. The next agent
	// process does not guess: it fails the step, brings the containers
	// back and reports.
	e.Start()

	op := e.Ctrl.AwaitFinished("lease-cut")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Contains(t, op.Error, "the agent restarted during phase applying")
	assert.Equal(t, protocol.StepFailed, agenttest.StepOf(t, op, needs[0].ID).Status)
	assert.Equal(t, protocol.StepPending, agenttest.StepOf(t, op, needs[1].ID).Status)

	calls := agenttest.Disruptive(e.Host.Calls())
	require.GreaterOrEqual(t, len(calls), 4)
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, calls[len(calls)-3:])
	assert.NotContains(t, strings.Join(calls, "\n"), "recreate teku")
}

// Stopping the agent (a restart of the unit, a redeployed binary) is not a
// crash: the step in flight and the steps after it are carried through,
// and the next process only has the waiting left.
func TestStopFinishesTheDisruptiveSteps(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")

	// The agent is told to stop right when the recreate of geth begins.
	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	var once sync.Once

	e.Host.OnCall(func(call string) {
		if strings.HasPrefix(call, "recreate geth") {
			once.Do(sigterm)
		}
	})

	svc := e.NewAgent()
	require.NoError(t, svc.Start(ctx))

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2), protocol.ImageNeedID("teku", agenttest.TekuV2))
	e.Ctrl.Grant(e.Lease("lease-stop", needs...))

	<-ctx.Done()
	require.NoError(t, svc.Stop())

	// Stop waited: both containers are recreated and running again. Only
	// the verifying wait was given up.
	assert.Equal(t, []string{
		"recreate geth " + agenttest.GethV2 + " start=true",
		"recreate teku " + agenttest.TekuV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, agenttest.Disruptive(e.Host.Calls()))
	assert.Equal(t, agenttest.GethV2, e.Host.Container("geth").Digest)
	assert.Equal(t, agenttest.TekuV2, e.Host.Container("teku").Digest)
	assert.Equal(t, protocol.StateRunning, e.Host.Container("teku").State)
	assert.Equal(t, "verifying", agenttest.Field(t, e.StateOnDisk(), "current", "operation", "phase"))

	e.Host.OnCall(nil)
	e.Start()

	op := e.Ctrl.AwaitFinished("lease-stop")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Empty(t, op.Error)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepDone, s.Status, s.NeedID)
	}

	recreates := 0

	for _, call := range e.Host.Calls() {
		if strings.HasPrefix(call, "recreate ") {
			recreates++
		}
	}

	assert.Equal(t, 2, recreates, "nothing is done twice")
}

// A stop during the drain must not leave the host drained and waiting: the
// reboot the drain was for is still issued.
func TestStopDuringTheDrainStillReboots(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := agenttest.NewEnv(t, sc)

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	var once sync.Once

	e.Host.OnCall(func(call string) {
		if call == "stop validator" {
			once.Do(sigterm)
		}
	})

	svc := e.NewAgent()
	require.NoError(t, svc.Start(ctx))

	needs := agenttest.AwaitNeeds(t, e, protocol.RebootNeedID)
	e.Ctrl.Grant(e.Lease("lease-drain", needs...))

	<-ctx.Done()
	require.NoError(t, svc.Stop())

	assert.Equal(t, []string{"stop validator", "stop teku", "stop geth", "reboot"},
		agenttest.Disruptive(e.Host.Calls()))
	assert.Equal(t, "boot-1", e.Host.BootID())
	assert.Equal(t, "rebooting", agenttest.Field(t, e.StateOnDisk(), "current", "operation", "phase"))

	e.Host.OnCall(nil)
	e.Start()

	op := e.Ctrl.AwaitFinished("lease-drain")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, protocol.RebootNeedID).Status)

	calls := agenttest.Disruptive(e.Host.Calls())
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, calls[len(calls)-3:])
}

// The cancel outlives the agent process. Two images; the lease is ended
// while the first is recreated, and the agent is stopped while it verifies.
// The process that takes the operation up from the state file reports the
// step that was never begun as cancelled, not as pending.
func TestCancelIsRememberedAcrossAnAgentRestart(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")

	entered, release := agenttest.BlockIn(e, "recreate geth")

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	svc := e.NewAgent()
	require.NoError(t, svc.Start(ctx))

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2), protocol.ImageNeedID("teku", agenttest.TekuV2))
	e.Ctrl.Grant(e.Lease("lease-cancel", needs...))

	agenttest.AwaitParked(t, entered, "recreate geth")
	agenttest.Cancel(t, e)

	// The agent is told to stop, and the recreate ends. Stop waits for the
	// step in progress and for the containers to be started; the verifying
	// wait is given up.
	sigterm()
	release()
	require.NoError(t, svc.Stop())

	require.Equal(t, "verifying", agenttest.Field(t, e.StateOnDisk(), "current", "operation", "phase"))
	require.Equal(t, true, agenttest.Field(t, e.StateOnDisk(), "current", "cancelled"))
	assert.NotContains(t, strings.Join(agenttest.Disruptive(e.Host.Calls()), "\n"), "recreate teku")

	// A new process finds the operation in its state file.
	e.Start()

	op := e.Ctrl.AwaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, needs[0].ID).Status)

	step := agenttest.StepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepCancelled, step.Status, "not begun because of the cancel, also after a restart")
	assert.Contains(t, step.Error, "cancelled")
	assert.NotContains(t, strings.Join(agenttest.Disruptive(e.Host.Calls()), "\n"), "recreate teku")
	assert.Equal(t, agenttest.TekuV1, e.Host.Container("teku").Digest)
	agenttest.AssertAllRunning(t, e)
}

// A person ends a lease while the agent is inside a disruptive call. That
// call is completed and counts as done; nothing else is begun, the steps
// that had not begun are reported cancelled, and what was stopped is started
// again by the path that brings a host back after a failed step.
func TestCancelDuringAnOperation(t *testing.T) {
	t.Parallel()

	const cancelled, done = protocol.StepCancelled, protocol.StepDone

	var (
		gethNeed = protocol.ImageNeedID("geth", agenttest.GethV2)
		tekuNeed = protocol.ImageNeedID("teku", agenttest.TekuV2)
		starts   = []string{"start geth", "start teku", "start validator"}
	)

	tests := []struct {
		name string
		// images are the tags that have moved; packages and reboot are the
		// other needs of the host.
		images   map[string]string
		packages bool
		reboot   bool
		// parkAt is the call the agent is inside when the cancel arrives.
		parkAt string
		needs  []string
		want   []protocol.StepStatus
		// geth is the digest of the execution client afterwards.
		geth  string
		check func(t *testing.T, calls []string)
	}{
		{
			// Two images and a reboot. The first disruptive call is the stop
			// of the validator client ahead of the reboot: no other container
			// is stopped, none is recreated, the host is not rebooted.
			name:   "in the first stop of the drain",
			images: map[string]string{agenttest.GethImage: agenttest.GethV2, agenttest.TekuImage: agenttest.TekuV2},
			reboot: true,
			parkAt: "stop validator",
			needs:  []string{gethNeed, tekuNeed, protocol.RebootNeedID},
			want:   []protocol.StepStatus{cancelled, cancelled, cancelled},
			geth:   agenttest.GethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				assert.Equal(t, starts, agenttest.CallsAfter(calls, "stop validator"),
					"after the call in progress the host is only brought back")
			},
		},
		{
			// Without a reboot the first disruptive call is the recreate of
			// the execution client; the consensus client is left alone.
			name:   "in a recreate",
			images: map[string]string{agenttest.GethImage: agenttest.GethV2, agenttest.TekuImage: agenttest.TekuV2},
			parkAt: "recreate geth",
			needs:  []string{gethNeed, tekuNeed},
			want:   []protocol.StepStatus{done, cancelled},
			geth:   agenttest.GethV2,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				joined := strings.Join(calls, "\n")
				assert.NotContains(t, joined, "recreate teku")
				assert.NotContains(t, joined, "stop ")
				assert.NotContains(t, joined, "reboot")
			},
		},
		{
			// The upgrade ends; the recreate that follows it is not begun.
			name:     "in the package upgrade, ahead of a recreate",
			images:   map[string]string{agenttest.GethImage: agenttest.GethV2},
			packages: true,
			parkAt:   "upgrade ",
			needs:    []string{agenttest.PackagesNeed().ID, gethNeed},
			want:     []protocol.StepStatus{done, cancelled},
			geth:     agenttest.GethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				assert.NotContains(t, strings.Join(calls, "\n"), "recreate")
			},
		},
		{
			// The host is drained. The upgrade ends; the reboot, which had
			// not begun, is not issued (operation.reboot asks under the state
			// lock), and the drained containers are started again.
			name:     "in the package upgrade, ahead of the reboot",
			packages: true,
			reboot:   true,
			parkAt:   "upgrade ",
			needs:    []string{agenttest.PackagesNeed().ID, protocol.RebootNeedID},
			want:     []protocol.StepStatus{done, cancelled},
			geth:     agenttest.GethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				assert.NotContains(t, calls, "reboot")
				assert.Equal(t, starts, agenttest.CallsAfter(calls, "upgrade "),
					"after the upgrade the host is only brought back")
			},
		},
		{
			// The execution client is the last container the drain stops.
			// That stop is completed; the upgrade is not begun
			// (operation.applyPackages asks first), nor is the reboot.
			name:     "between the drain and the package upgrade",
			packages: true,
			reboot:   true,
			parkAt:   "stop geth",
			needs:    []string{agenttest.PackagesNeed().ID, protocol.RebootNeedID},
			want:     []protocol.StepStatus{cancelled, cancelled},
			geth:     agenttest.GethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				assert.Equal(t, append([]string{"stop validator", "stop teku", "stop geth"}, starts...),
					calls, "drained, then only brought back: no upgrade, no reboot")
			},
		},
		{
			// Nothing is left to keep from beginning: the operation ends as
			// it would have.
			name:   "after the last step has begun",
			images: map[string]string{agenttest.GethImage: agenttest.GethV2},
			parkAt: "recreate geth",
			needs:  []string{gethNeed},
			want:   []protocol.StepStatus{done},
			geth:   agenttest.GethV2,
			check:  func(*testing.T, []string) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sc := agenttest.BaseScenario()
			sc.RebootRequired = tt.reboot

			e := agenttest.NewEnv(t, sc)

			for image, digest := range tt.images {
				e.Host.SetRegistry(image, digest, "")
			}

			if tt.packages {
				e.Host.SetUpgradable(agenttest.DockerUpgrade())
			}

			entered, release := agenttest.BlockIn(e, tt.parkAt)

			e.Start()

			needs := agenttest.AwaitNeeds(t, e, tt.needs...)
			e.Ctrl.Grant(e.Lease("lease-cancel", needs...))

			agenttest.AwaitParked(t, entered, tt.parkAt)
			agenttest.Cancel(t, e)
			release()

			op := e.Ctrl.AwaitFinished("lease-cancel")

			assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
			assert.True(t, op.Disrupted, "the host was touched")

			for i, id := range tt.needs {
				step := agenttest.StepOf(t, op, id)
				assert.Equal(t, tt.want[i], step.Status, id)

				if tt.want[i] == cancelled {
					assert.Contains(t, step.Error, "cancelled", id)
				}
			}

			tt.check(t, agenttest.Disruptive(e.Host.Calls()))

			agenttest.AssertAllRunning(t, e)
			assert.Equal(t, tt.geth, e.Host.Container("geth").Digest)
			assert.Equal(t, agenttest.TekuV1, e.Host.Container("teku").Digest)
			assert.Equal(t, "boot", e.Host.BootID(), "no reboot")
		})
	}
}

// What the agent reports does not depend on whether it was restarted. The
// recreate of the first of two images fails and the lease is ended while it
// runs; the agent is stopped before it could finish (the start of the
// execution client keeps failing until then) and a new process ends the
// operation from the state file. The step that was never begun is reported
// as after any failure (pending, not attempted): it is reported as cancelled
// only when the cancel is what stopped the operation.
func TestReportAfterAFailedStepDoesNotDependOnARestart(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")
	e.Host.Fail("recreate:geth", errors.New("no space left on device"))
	e.Host.Fail("start:geth", errors.New("docker daemon is restarting"))

	entered, release := agenttest.BlockIn(e, "recreate geth")

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	svc := e.NewAgent()
	require.NoError(t, svc.Start(ctx))

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2), protocol.ImageNeedID("teku", agenttest.TekuV2))
	e.Ctrl.Grant(e.Lease("lease-cancel", needs...))

	agenttest.AwaitParked(t, entered, "recreate geth")
	agenttest.Cancel(t, e)

	sigterm()
	release()
	require.NoError(t, svc.Stop())

	require.Equal(t, "starting", agenttest.Field(t, e.StateOnDisk(), "current", "operation", "phase"))
	require.Equal(t, true, agenttest.Field(t, e.StateOnDisk(), "current", "cancelled"))

	e.Host.Fail("start:geth", nil)
	e.Start()

	op := e.Ctrl.AwaitFinished("lease-cancel")

	assert.NotContains(t, strings.Join(agenttest.Disruptive(e.Host.Calls()), "\n"), "recreate teku")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Equal(t, protocol.StepFailed, agenttest.StepOf(t, op, needs[0].ID).Status)

	step := agenttest.StepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepPending, step.Status,
		"not begun because a step failed: reported as after any failure")
	assert.Contains(t, step.Error, "not attempted")
}
