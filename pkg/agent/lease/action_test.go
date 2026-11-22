package lease

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	testAction     = "security-upgrades"
	testActionPath = "/usr/local/lib/upgradoor/actions/security-upgrades"
)

// actionEnv is an env whose policy has the security-upgrades action and
// whose host has a security update outside the blocked set.
func actionEnv(t *testing.T) *env {
	t.Helper()

	e := newEnv(t, agenttest.BaseScenario())
	e.policy.Actions = map[string]plan.Action{testAction: {
		Command: []string{testActionPath}, Impact: protocol.ImpactIndependent,
		Timeout: time.Minute, When: protocol.ConditionSecurityUpdates,
	}}
	e.host.SetUpgradable([]protocol.Upgradable{
		{Name: "openssl", Arch: "amd64", Current: "3.5.1", Candidate: "3.5.2", Security: true},
	})

	return e
}

func actionNeed() protocol.Need {
	return protocol.Need{
		ID: protocol.ActionNeedID(testAction), Kind: protocol.NeedAction,
		Action: &protocol.ActionNeed{
			Name: testAction, Impact: protocol.ImpactIndependent, When: protocol.ConditionSecurityUpdates,
		},
	}
}

func TestActionRuns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		result  *protocol.ActionResult
		phase   protocol.Phase
		status  protocol.StepStatus
		failure string
	}{
		{name: "success", phase: protocol.PhaseDone, status: protocol.StepDone},
		{
			name:   "non-zero exit",
			result: &protocol.ActionResult{ExitCode: 3, Output: "E: Could not get lock"},
			phase:  protocol.PhaseFailed, status: protocol.StepFailed,
			failure: "action security-upgrades exited with code 3",
		},
		{
			name:   "timeout",
			result: &protocol.ActionResult{ExitCode: -1, TimedOut: true},
			phase:  protocol.PhaseFailed, status: protocol.StepFailed,
			failure: "action security-upgrades timed out after 1m0s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := actionEnv(t)
			e.host.SetActionResult(tt.result)
			e.start(t.Context())

			need := actionNeed()
			e.grant("lease-action", need)

			op := e.awaitFinished("lease-action")
			require.Equal(t, tt.phase, op.Phase, op.Error)
			assert.True(t, op.Disrupted, "an action that ran counts as work on the host")
			assert.Equal(t, tt.failure, op.Error)

			step := stepOf(t, op, need.ID)
			assert.Equal(t, tt.status, step.Status)
			assert.Equal(t, tt.failure, step.Error)
			require.NotNil(t, step.Action)

			want := protocol.ActionResult{}
			if tt.result != nil {
				want = *tt.result
			}

			assert.Equal(t, want, *step.Action)

			// The argv is agent.yaml's; the packages are rescanned right after.
			calls := e.host.Calls()
			at := slices.Index(calls, "action "+testActionPath)
			require.GreaterOrEqual(t, at, 0, calls)
			assert.Equal(t, "rescan", calls[at+1])
			assert.Equal(t, protocol.PhaseAction, e.phasesOf("lease-action")[0])

			e.mu.Lock()
			ran := e.ran[testAction]
			e.mu.Unlock()
			assert.NotEmpty(t, ran, "the inventory hash after the run is handed on")
		})
	}
}

// An observed need whose condition no longer holds, or that names an action
// agent.yaml does not have, runs nothing.
func TestActionNotRun(t *testing.T) {
	t.Parallel()

	unknown := actionNeed()
	unknown.ID, unknown.Action.Name = protocol.ActionNeedID("rm-rf"), "rm-rf"

	tests := []struct {
		name   string
		need   protocol.Need
		setup  func(e *env)
		status protocol.StepStatus
		reason string
	}{
		{
			name: "condition gone", need: actionNeed(),
			setup:  func(e *env) { e.host.SetUpgradable(nil) },
			status: protocol.StepSuperseded,
			reason: "the condition of action security-upgrades no longer holds",
		},
		{
			name: "unknown action", need: unknown, setup: func(*env) {},
			status: protocol.StepRefused, reason: `unknown action "rm-rf"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := actionEnv(t)
			tt.setup(e)
			e.start(t.Context())
			e.grant("lease-none", tt.need)

			op := e.awaitFinished("lease-none")
			assert.Equal(t, protocol.PhaseDone, op.Phase)
			assert.False(t, op.Disrupted)

			step := stepOf(t, op, tt.need.ID)
			assert.Equal(t, tt.status, step.Status)
			assert.Equal(t, tt.reason, step.Error)
			assert.Empty(t, e.disruptive())
		})
	}
}

// A manual need runs the action the agent stored for the directive, whatever
// name the lease's copy carries.
func TestManualActionRunsTheStoredAction(t *testing.T) {
	t.Parallel()

	e := actionEnv(t)
	e.host.SetUpgradable(nil)

	stored := protocol.Need{
		ID: protocol.ManualNeedID("d-1"), Kind: protocol.NeedAction, DirectiveID: "d-1",
		Action: &protocol.ActionNeed{Name: testAction, Impact: protocol.ImpactIndependent},
	}
	e.addManual(stored)
	e.start(t.Context())

	granted := stored
	granted.Action = &protocol.ActionNeed{Name: "something-else", Impact: protocol.ImpactIndependent}
	e.grant("lease-manual", granted)

	op := e.awaitFinished("lease-manual")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, stored.ID).Status)
	assert.Contains(t, e.host.Calls(), "action "+testActionPath)
}

// The command outlives an agent that is killed while it runs: the next
// process does not know how it ended and fails the step as unknown.
func TestKilledActionIsResumedAsUnknown(t *testing.T) {
	t.Parallel()

	e := actionEnv(t)
	entered, _ := e.block("action ")
	e.start(t.Context())

	need := actionNeed()
	e.grant("lease-cut", need)

	agenttest.AwaitParked(t, entered, "action")
	assert.Equal(t, string(protocol.PhaseAction), field(t, e.onDisk(), "current", "operation", "phase"))

	// The first runner stays parked in the call: as dead as it gets.
	e.start(t.Context())

	op := e.awaitFinished("lease-cut")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Equal(t, reasonActionUnknown, op.Error)

	step := stepOf(t, op, need.ID)
	assert.Equal(t, protocol.StepFailed, step.Status)
	assert.Equal(t, reasonActionUnknown, step.Error)
	require.NotNil(t, step.Action)
	assert.True(t, step.Action.Unknown)
	assert.Equal(t, -1, step.Action.ExitCode)
}
