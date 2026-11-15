package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// parkLimit bounds how long a parked host call waits for its release. A test
// that fails while a call is parked is over long before; the limit only keeps
// a forgotten release from holding the agent's Stop until the package's
// timeout.
const parkLimit = 3 * awaitTimeout

// blockIn parks the first host call with the given prefix until the returned
// release is called; entered is closed once the call is parked. A test that
// has failed releases the call by itself, so that the agent can be stopped
// and the failure is the one that is reported.
func blockIn(e *env, prefix string) (entered <-chan struct{}, release func()) {
	var (
		parked atomic.Bool
		in     = make(chan struct{})
		out    = make(chan struct{})
		opened atomic.Bool
	)

	e.host.OnCall(func(call string) {
		if !strings.HasPrefix(call, prefix) || !parked.CompareAndSwap(false, true) {
			return
		}

		close(in)

		limit := time.After(parkLimit)
		tick := time.NewTicker(20 * time.Millisecond)

		defer tick.Stop()

		for {
			select {
			case <-out:
				return
			case <-limit:
				return
			case <-tick.C:
				if e.t.Failed() {
					return
				}
			}
		}
	})

	release = func() {
		if opened.CompareAndSwap(false, true) {
			close(out)
		}
	}

	e.t.Cleanup(release)

	return in, release
}

// awaitParked waits until the agent is inside the call that blockIn parks,
// and fails by name when it never gets there.
func awaitParked(t *testing.T, entered <-chan struct{}, call string) {
	t.Helper()

	select {
	case <-entered:
	case <-time.After(awaitTimeout):
		require.FailNow(t, "the agent never began the call: "+call)
	}
}

// cancel restates the controller's lease with the cancel mark and waits until
// the agent has taken it in.
func cancel(t *testing.T, e *env) {
	t.Helper()

	e.ctrl.set(func(c *controller) {
		l := *c.lease
		l.Cancel = true
		c.lease = &l
	})

	require.Eventually(t, func() bool {
		cur, ok := e.stateOnDisk()["current"].(map[string]any)

		return ok && cur["cancelled"] == true
	}, awaitTimeout, 2*e.cfg.Controller.LeaseSyncInterval,
		"the agent did not take in that its lease was cancelled (service.handleLease, service.cancelLease)")
}

// callsAfter returns the disruptive calls that follow the first one with the
// prefix.
func callsAfter(calls []string, prefix string) []string {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return calls[i+1:]
		}
	}

	return nil
}

func assertAllRunning(t *testing.T, e *env) {
	t.Helper()

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.Equal(t, protocol.StateRunning, e.host.Container(name).State, name)
	}
}

// The lease of the review: two images and a reboot. The agent is inside its
// first disruptive call (the stop of the validator client ahead of the reboot)
// when a person ends the lease. That call is completed; nothing else is
// begun: no other container is stopped, none is recreated, the host is not
// rebooted. The container that was stopped is started again by the path that
// brings a host back after a failed step.
func TestCancelStopsAnOperationAfterTheCallInProgress(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")
	e.host.SetRebootRequired(true)

	entered, release := blockIn(e, "stop validator")

	e.start()

	needs := awaitNeeds(t, e,
		protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2), protocol.RebootNeedID)
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, "stop validator")
	cancel(t, e)
	release()

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted, "a container was stopped")

	for _, n := range needs {
		step := stepOf(t, op, n.ID)
		assert.Equal(t, protocol.StepCancelled, step.Status, n.ID)
		assert.Contains(t, step.Error, "cancelled")
	}

	after := callsAfter(disruptive(e.host.Calls()), "stop validator")
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, after,
		"after the call in progress the host is only brought back")

	assertAllRunning(t, e)
	assert.Equal(t, gethV1, e.host.Container("geth").Digest)
	assert.Equal(t, tekuV1, e.host.Container("teku").Digest)
	assert.Equal(t, "boot", e.host.BootID(), "no reboot")
}

// Without a reboot the first disruptive call is the recreate of the execution
// client. It is completed and counts as done; the consensus client, whose
// step had not begun, is left alone and its step is reported cancelled.
func TestCancelCompletesTheStepInProgressAndBeginsNoOther(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")

	entered, release := blockIn(e, "recreate geth")

	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, "recreate geth")
	cancel(t, e)
	release()

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[0].ID).Status)
	assert.Equal(t, protocol.StepCancelled, stepOf(t, op, needs[1].ID).Status)

	calls := strings.Join(disruptive(e.host.Calls()), "\n")
	assert.NotContains(t, calls, "recreate teku")
	assert.NotContains(t, calls, "stop ")
	assert.NotContains(t, calls, "reboot")

	assertAllRunning(t, e)
	assert.Equal(t, gethV2, e.host.Container("geth").Digest, "the step in progress was completed")
	assert.Equal(t, tekuV1, e.host.Container("teku").Digest)
}

// A cancel that arrives while the package upgrade runs lets the upgrade end
// and keeps the recreate that follows it from being begun.
func TestCancelDuringThePackageUpgradeBeginsNoRecreate(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetUpgradable(dockerUpgrade())
	e.host.SetRegistry(gethImage, gethV2, "")

	entered, release := blockIn(e, "upgrade ")

	e.start()

	needs := awaitNeeds(t, e, packagesNeed().ID, protocol.ImageNeedID("geth", gethV2))
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, "upgrade")
	cancel(t, e)
	release()

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[0].ID).Status)
	assert.Equal(t, protocol.StepCancelled, stepOf(t, op, needs[1].ID).Status)
	assert.NotContains(t, strings.Join(disruptive(e.host.Calls()), "\n"), "recreate")
	assertAllRunning(t, e)
}

// A lease with a reboot: the host is drained, the packages are being upgraded
// when a person ends the lease. The upgrade ends; the reboot, which had not
// begun, is not issued (operation.reboot asks under the state lock), and the
// drained containers are started again.
func TestCancelDuringThePackageUpgradeKeepsTheRebootAway(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.host.SetUpgradable(dockerUpgrade())

	entered, release := blockIn(e, "upgrade ")

	e.start()

	needs := awaitNeeds(t, e, packagesNeed().ID, protocol.RebootNeedID)
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, "upgrade")
	cancel(t, e)
	release()

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, packagesNeed().ID).Status, "begun, so completed")
	assert.Equal(t, protocol.StepCancelled, stepOf(t, op, protocol.RebootNeedID).Status)

	calls := disruptive(e.host.Calls())
	assert.NotContains(t, calls, "reboot")
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, callsAfter(calls, "upgrade "),
		"after the upgrade the host is only brought back")
	assert.Equal(t, "boot", e.host.BootID(), "no reboot")
	assertAllRunning(t, e)
}

// The cancel arrives between the drain and the package upgrade: the agent is
// inside the last stop of the drain when it takes the cancel in. That stop is
// completed; the upgrade is not begun (operation.applyPackages asks first),
// nor is the reboot.
func TestCancelBetweenTheDrainAndThePackageUpgradeBeginsNoUpgrade(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.host.SetUpgradable(dockerUpgrade())

	// The execution client is the last container the drain stops.
	entered, release := blockIn(e, "stop geth")

	e.start()

	needs := awaitNeeds(t, e, packagesNeed().ID, protocol.RebootNeedID)
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, "stop geth")
	cancel(t, e)
	release()

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted, "the containers were stopped")
	assert.Equal(t, protocol.StepCancelled, stepOf(t, op, packagesNeed().ID).Status)
	assert.Equal(t, protocol.StepCancelled, stepOf(t, op, protocol.RebootNeedID).Status)

	calls := disruptive(e.host.Calls())
	assert.Equal(t, []string{
		"stop validator", "stop teku", "stop geth", "start geth", "start teku", "start validator",
	}, calls, "drained, then only brought back: no upgrade, no reboot")
	assert.Equal(t, "boot", e.host.BootID())
	assertAllRunning(t, e)
}

// L1: the cancel outlives the agent process. Two images; the lease is ended
// while the first is recreated, and the agent is stopped while it verifies.
// The process that takes the operation up from the state file reports the
// step that was never begun as cancelled, not as pending.
func TestCancelIsRememberedAcrossAnAgentRestart(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")

	entered, release := blockIn(e, "recreate geth")

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	svc := e.newAgent()
	require.NoError(t, svc.Start(ctx))

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, "recreate geth")
	cancel(t, e)

	// The agent is told to stop, and the recreate ends. Stop waits for the
	// step in progress and for the containers to be started; the verifying
	// wait is given up.
	sigterm()
	release()
	require.NoError(t, svc.Stop())

	require.Equal(t, "verifying", field(t, e.stateOnDisk(), "current", "operation", "phase"))
	require.Equal(t, true, field(t, e.stateOnDisk(), "current", "cancelled"))
	assert.NotContains(t, strings.Join(disruptive(e.host.Calls()), "\n"), "recreate teku")

	// A new process finds the operation in its state file.
	e.start()

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[0].ID).Status)

	step := stepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepCancelled, step.Status, "not begun because of the cancel, also after a restart")
	assert.Contains(t, step.Error, "cancelled")
	assert.NotContains(t, strings.Join(disruptive(e.host.Calls()), "\n"), "recreate teku")
	assert.Equal(t, tekuV1, e.host.Container("teku").Digest)
	assertAllRunning(t, e)
}

// A lease that is cancelled before the agent has started it is never
// started. The agent says so at once, so the controller need not wait for
// the lease to run out.
func TestCancelledLeaseIsNotStarted(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.ctrl.set(func(c *controller) { c.autoRelease = false })
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	// The host cannot be examined: the agent holds the lease and has not
	// started it.
	e.host.Fail("observe", errors.New("docker daemon is restarting"))
	e.ctrl.grant(e.lease("lease-cancel", needs...))
	awaitLeaseOnDisk(t, e, "lease-cancel")

	cancel(t, e)
	e.host.Fail("observe", nil)

	req := e.ctrl.await("the refusal", func(req *protocol.SyncRequest) bool {
		return req.Operation != nil && req.Operation.LeaseID == "lease-cancel" && req.Operation.Phase.Terminal()
	})
	op := req.Operation

	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, stepOf(t, *op, needs[0].ID).Status)
	assert.Contains(t, stepOf(t, *op, needs[0].ID).Error, "cancelled before the operation started")
	assert.Empty(t, disruptive(e.host.Calls()), "nothing was touched")
}

// A lease that the agent first hears of with the cancel mark is not taken.
func TestLeaseOfferedCancelledIsNotTaken(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.ctrl.set(func(c *controller) { c.autoRelease = false })
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	l := e.lease("lease-cancel", needs...)
	l.Cancel = true
	e.ctrl.grant(l)
	e.ctrl.awaitSyncs(e.ctrl.syncCount() + 6)

	for _, s := range e.ctrl.allSyncs() {
		require.Nil(t, s.Operation)
	}

	assert.Nil(t, e.stateOnDisk()["current"])
	assert.Empty(t, disruptive(e.host.Calls()))
}

// A cancel that arrives after the last step has begun changes nothing: the
// operation ends as it would have.
func TestCancelAfterTheLastStepHasBegunChangesNothing(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")

	entered, release := blockIn(e, "recreate geth")

	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, "recreate geth")
	cancel(t, e)
	release()

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[0].ID).Status)
	assert.Equal(t, gethV2, e.host.Container("geth").Digest)
	assertAllRunning(t, e)
}

// failedUnderCancel runs a lease of two images in which the recreate of the
// first fails and the lease is ended, and returns the final report. The
// cancel arrives while the failing recreate runs, or after it, while the
// containers are started again. With restart the agent is stopped before it
// could finish (the start of the execution client keeps failing until then)
// and a new process ends the operation from the state file.
func failedUnderCancel(t *testing.T, parkAt string, restart bool) protocol.Operation {
	t.Helper()

	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")
	e.host.Fail("recreate:geth", errors.New("no space left on device"))

	if restart {
		e.host.Fail("start:geth", errors.New("docker daemon is restarting"))
	}

	entered, release := blockIn(e, parkAt)

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	svc := e.newAgent()
	require.NoError(t, svc.Start(ctx))

	stopped := false

	t.Cleanup(func() {
		if !stopped {
			assert.NoError(t, svc.Stop())
		}
	})

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, parkAt)
	cancel(t, e)

	if restart {
		sigterm()
		release()
		require.NoError(t, svc.Stop())

		stopped = true

		require.Equal(t, "starting", field(t, e.stateOnDisk(), "current", "operation", "phase"))
		require.Equal(t, true, field(t, e.stateOnDisk(), "current", "cancelled"))

		e.host.Fail("start:geth", nil)
		e.start()
	} else {
		release()
	}

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.NotContains(t, strings.Join(disruptive(e.host.Calls()), "\n"), "recreate teku")

	return op
}

// L3 of the review of the seventh round: what the agent reports does not
// depend on whether it was restarted. A step failed and the lease was ended;
// the step that was never begun is reported as after any failure (pending,
// not attempted), by the process that began the operation and by one that
// takes it up from the state file alike, whenever the cancel arrived. It is
// reported as cancelled only when the cancel is what stopped the operation.
func TestReportAfterAFailedStepDoesNotDependOnARestart(t *testing.T) {
	type report struct {
		phase protocol.Phase
		err   string
		steps []protocol.Step
	}

	var first *report

	for _, parkAt := range []string{"recreate geth", "start geth"} {
		for _, restart := range []bool{false, true} {
			name := "cancel during " + parkAt
			if restart {
				name += ", the agent is restarted"
			}

			t.Run(name, func(t *testing.T) {
				op := failedUnderCancel(t, parkAt, restart)
				got := &report{phase: op.Phase, err: op.Error, steps: op.Steps}

				require.Len(t, got.steps, 2)
				assert.Equal(t, protocol.PhaseFailed, got.phase)
				assert.Equal(t, protocol.StepFailed, got.steps[0].Status)
				assert.Equal(t, protocol.StepPending, got.steps[1].Status,
					"not begun because a step failed: reported as after any failure")
				assert.Contains(t, got.steps[1].Error, "not attempted")

				if first == nil {
					first = got
				}

				assert.Equal(t, first, got, "the same report in every order")
			})
		}
	}
}
