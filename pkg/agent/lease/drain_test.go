package lease

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestRebootOrder(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()
	sc.RebootRequired = true
	sc.NewestKernel = "6.12.48-amd64"

	e := newEnv(t, sc)
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)
	e.start(t.Context())

	e.grant("lease-reboot", tekuNeed(), agenttest.RebootNeed(), gethNeed(agenttest.GethV2),
		agenttest.PackagesNeed())

	op := e.awaitFinished("lease-reboot")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepDone, s.Status, s.NeedID)
	}

	// One downtime: drain VC, CL, EL; upgrade; recreate stopped; reboot; start EL, CL, VC.
	assert.Equal(t, []string{
		"stop validator", "stop teku", "stop geth",
		"upgrade docker-ce",
		"recreate geth " + agenttest.GethV2 + " start=false",
		"recreate teku " + agenttest.TekuV2 + " start=false",
		"reboot",
		"start geth", "start teku", "start validator",
	}, e.disruptive())

	assert.Equal(t, "boot-1", e.host.BootID())

	for _, name := range []string{"geth", "teku", "validator", "dora"} {
		assert.Equal(t, protocol.StateRunning, e.host.Container(name).State, name)
	}

	e.assertPhases("lease-reboot", protocol.PhaseDraining, protocol.PhaseApplying,
		protocol.PhaseRebooting, protocol.PhaseStarting, protocol.PhaseVerifying, protocol.PhaseDone)
}

// timeoutHost records the stop timeout of every stop and recreate.
type timeoutHost struct {
	*agenttest.Host

	mu    sync.Mutex
	calls []string
}

func (h *timeoutHost) note(call string, timeout time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.calls = append(h.calls, fmt.Sprintf("%s %s", call, timeout))
}

func (h *timeoutHost) got() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.calls)
}

func (h *timeoutHost) Stop(ctx context.Context, name string, timeout time.Duration) error {
	h.note("stop "+name, timeout)

	return h.Host.Stop(ctx, name, timeout)
}

func (h *timeoutHost) Recreate(
	ctx context.Context, name, digest string, timeout time.Duration, start bool, change *host.Change,
) error {
	h.note("recreate "+name, timeout)

	return h.Host.Recreate(ctx, name, digest, timeout, start, change)
}

// Drain and recreate use the stop timeout of the container's classified group.
func TestStopTimeoutsOfTheClassifiedContainer(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true
	sc.Containers[1].Group, sc.Containers[1].Kind = "", ""

	e := newEnv(t, sc)
	e.policy.Containers = map[string]plan.MappedContainer{
		"geth": {Group: agenttest.GroupExecution, Kind: protocol.KindEL},
	}
	e.policy.Groups[agenttest.GroupExecution] = plan.Group{StopTimeout: 30 * time.Second}

	recording := &timeoutHost{Host: e.host}
	e.machine = recording
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.start(t.Context())

	e.grant("lease-1", agenttest.RebootNeed(), gethNeed(agenttest.GethV2))

	op := e.awaitFinished("lease-1")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, []string{
		"stop validator 5m0s", "stop teku 5m0s", "stop geth 30s", "recreate geth 30s",
	}, recording.got())
}
