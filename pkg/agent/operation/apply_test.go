package operation_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestDigestMismatchSupersedesWhenTheTagMovedOn(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Start()

	stale := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	// The tag moves again before the slot comes; the agent pulls the newer
	// image, so the local tag no longer is the digest of the old need.
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV3, "")
	agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV3))

	e.Ctrl.Grant(e.Lease("lease-stale", stale...))

	op := e.Ctrl.AwaitFinished("lease-stale")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted, "a superseded step costs nothing")
	assert.Empty(t, op.Error)

	step := agenttest.StepOf(t, op, stale[0].ID)
	assert.Equal(t, protocol.StepSuperseded, step.Status)
	assert.Contains(t, step.Error, agenttest.GethV3)

	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()))
	assert.Equal(t, agenttest.GethV1, e.Host.Container("geth").Digest)

	// The need for the newer digest is still open.
	req := e.Ctrl.Await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })
	assert.Equal(t, []string{protocol.ImageNeedID("geth", agenttest.GethV3)}, agenttest.NeedIDs(&req))
}

func TestDigestMismatchFailsWhenTheTagStillPointsThere(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())

	// The registry has geth-2, but the image never arrives locally. A lease
	// for geth-2 must not make the agent run whatever is local.
	e.Host.Fail("pull:"+agenttest.GethImage, errors.New("no space left on device"))
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Start()

	e.Ctrl.Await("failed pull", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Registry != nil && c.Registry.Error != ""
	})

	need := agenttest.ImageNeedFor("geth", agenttest.GethImage, agenttest.GethV1, agenttest.GethV2)
	e.Ctrl.Grant(e.Lease("lease-unpulled", need))

	op := e.Ctrl.AwaitFinished("lease-unpulled")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.False(t, op.Disrupted, "the digest is checked before anything stops")
	assert.Contains(t, op.Error, "although the tag still points at it")

	step := agenttest.StepOf(t, op, need.ID)
	assert.Equal(t, protocol.StepFailed, step.Status)
	assert.Equal(t, op.Error, step.Error)

	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()))
	assert.Equal(t, agenttest.GethV1, e.Host.Container("geth").Digest)
	assert.Equal(t, protocol.StateRunning, e.Host.Container("geth").State)
}

// A container that the name map of agent.yaml puts in its group gets the
// labels with its next recreate; one of the default group is rolled only
// where agent.yaml says so, and keeps having no labels.
func TestRecreateLabelsMappedContainers(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Containers[1].Group, sc.Containers[1].Kind = "", ""
	sc.Containers[3].Group = ""

	e := agenttest.NewEnv(t, sc)
	e.Cfg.Containers = map[string]config.MappedContainer{
		"geth": {Group: agenttest.GroupExecution, Kind: protocol.KindEL},
	}
	e.Cfg.DefaultGroup.Roll = true
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry("ethpandaops/dora:latest", "sha256:dora-2", "")
	e.Start()

	needs := agenttest.AwaitNeeds(t, e,
		protocol.ImageNeedID("dora", "sha256:dora-2"),
		protocol.ImageNeedID("geth", agenttest.GethV2),
	)
	assert.Equal(t, protocol.DefaultGroup, needs[0].Image.Group)
	assert.Equal(t, agenttest.GroupExecution, needs[1].Image.Group)

	e.Ctrl.Grant(e.Lease("lease-mapped", needs...))

	op := e.Ctrl.AwaitFinished("lease-mapped")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	geth := e.Host.Container("geth")
	assert.Equal(t, agenttest.GethV2, geth.Digest)
	assert.Equal(t, agenttest.GroupExecution, geth.Group, "the recreate set the group label")
	assert.Equal(t, protocol.KindEL, geth.Kind)

	dora := e.Host.Container("dora")
	assert.Equal(t, "sha256:dora-2", dora.Digest)
	assert.Empty(t, dora.Group, "the default group is no label")

	req := e.Ctrl.Await("labelled", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Equal(t, protocol.DefaultGroup, agenttest.ContainerOf(t, &req, "dora").Group)
	assert.Equal(t, protocol.ImpactIndependent, agenttest.ContainerOf(t, &req, "dora").Impact)
}

func TestRollOrder(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()

	e := agenttest.NewEnv(t, sc)
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")
	e.Host.SetRegistry(agenttest.VCImage, agenttest.VCV2, "")
	e.Start()

	// Granted in an order that is not the order of execution.
	needs := agenttest.AwaitNeeds(t, e,
		protocol.ImageNeedID("validator", agenttest.VCV2),
		protocol.ImageNeedID("teku", agenttest.TekuV2),
		agenttest.PackagesNeed().ID,
		protocol.ImageNeedID("geth", agenttest.GethV2),
	)
	e.Ctrl.Grant(e.Lease("lease-all", needs...))

	op := e.Ctrl.AwaitFinished("lease-all")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	require.Len(t, op.Steps, 4)

	for i, s := range op.Steps {
		assert.Equal(t, needs[i].ID, s.NeedID, "steps keep the order of the lease")
		assert.Equal(t, protocol.StepDone, s.Status)
	}

	// Packages first, then EL, CL, VC; nothing is drained without a reboot.
	assert.Equal(t, []string{
		"upgrade docker-ce",
		"recreate geth " + agenttest.GethV2 + " start=true",
		"recreate teku " + agenttest.TekuV2 + " start=true",
		"recreate validator " + agenttest.VCV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, agenttest.Disruptive(e.Host.Calls()))

	assert.Equal(t, agenttest.GethV2, e.Host.Container("geth").Digest)
	assert.Equal(t, "sha256:dora-1", e.Host.Container("dora").Digest)

	req := e.Ctrl.Await("all served", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Zero(t, req.Pending.Blocked)

	// The phases were reported while it ran, in order and never backwards.
	assertPhaseOrder(t, e, "lease-all", protocol.PhaseDone)
}
