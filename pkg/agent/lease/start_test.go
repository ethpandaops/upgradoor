package lease

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestFailedStepStillRestartsContainers(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)
	e.pull(agenttest.VCImage, agenttest.VCV2)
	e.host.Fail("recreate:teku", errors.New("port is already allocated"))
	e.start(t.Context())

	needs := []protocol.Need{vcNeed(), tekuNeed(), gethNeed(agenttest.GethV2)}
	e.grant("lease-broken", needs...)

	op := e.awaitFinished("lease-broken")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.True(t, op.Disrupted)
	assert.Contains(t, op.Error, "recreate container teku")
	assert.Contains(t, op.Error, "port is already allocated")

	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[2].ID).Status, "geth came first and worked")

	teku := stepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepFailed, teku.Status)
	assert.Contains(t, teku.Error, "port is already allocated")

	vc := stepOf(t, op, needs[0].ID)
	assert.Equal(t, protocol.StepPending, vc.Status, "nothing is attempted after a failure")
	assert.Contains(t, vc.Error, "not attempted")

	// Everything that ran before is started; nothing is rolled back.
	assert.Equal(t, []string{
		"recreate geth " + agenttest.GethV2 + " start=true",
		"recreate teku " + agenttest.TekuV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, e.disruptive())

	assert.Equal(t, protocol.StateRunning, e.host.Container("teku").State)
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest, "no rollback")
	assert.Equal(t, agenttest.VCV1, e.host.Container("validator").Digest)

	// No verifying after a failed step.
	e.assertPhases("lease-broken", protocol.PhaseApplying, protocol.PhaseStarting, protocol.PhaseFailed)
}

// A crashed container comes back with its roll; a stopped one stays down.
func TestRollStartsACrashedContainerButNotAStoppedOne(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())

	require.NoError(t, e.host.Exit("teku", 1))
	require.NoError(t, e.host.Exit("validator", 143))

	e.pull(agenttest.TekuImage, agenttest.TekuV2)
	e.pull(agenttest.VCImage, agenttest.VCV2)
	e.start(t.Context())

	e.grant("lease-repair", tekuNeed(), vcNeed())

	op := e.awaitFinished("lease-repair")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	calls := e.disruptive()
	assert.Contains(t, calls, "recreate teku "+agenttest.TekuV2+" start=true")
	assert.Contains(t, calls, "recreate validator "+agenttest.VCV2+" start=false")
	assert.NotContains(t, calls, "start validator")

	assert.Equal(t, protocol.StateRunning, e.host.Container("teku").State)
	assert.Equal(t, "created", e.host.Container("validator").State, "recreated, not started")
	assert.Equal(t, agenttest.VCV2, e.host.Container("validator").Digest, "rolled all the same")
}

// Ending an operation closes the manual needs it served; a decline or an empty plan does not.
func TestFinishClosesTheServedManualNeeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// container is the one the manual roll is for.
		container string
		failed    bool
		expired   bool
		open      bool
	}{
		{name: "done", container: "geth"},
		{name: "failed", container: "geth", failed: true},
		{name: "declined", container: "geth", expired: true, open: true},
		{name: "empty plan", container: "dora", open: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEnv(t, agenttest.BaseScenario())
			e.pull(agenttest.GethImage, agenttest.GethV2)

			if tt.failed {
				e.host.Fail("recreate:geth", errors.New("no space left on device"))
			}

			need := protocol.Need{
				ID: protocol.LocalNeedID("r-1"), Kind: protocol.NeedImage, Local: true,
				Image: &protocol.ImageNeed{
					Container: tt.container, Image: agenttest.GethImage,
					FromDigest: agenttest.GethV1, ToDigest: agenttest.GethV2,
				},
			}
			e.addManual(need)
			e.start(t.Context())

			l := e.lease("lease-1", need)
			if tt.expired {
				l.ExpiresAt = e.now().Add(-time.Second)
			}

			require.True(t, e.follow(l))
			e.awaitFinished("lease-1")

			want := []protocol.Need{need}
			if !tt.open {
				want = nil
			}

			assert.ElementsMatch(t, want, e.state.Manual())

			reopened, err := OpenState(quietLog(), e.dir)
			require.NoError(t, err)
			assert.ElementsMatch(t, want, reopened.Manual())
		})
	}
}
