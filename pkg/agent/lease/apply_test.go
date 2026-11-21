package lease

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestDigestMismatchSupersedesWhenTheTagMovedOn(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	// The tag moved on again and the newer image is local.
	e.pull(agenttest.GethImage, agenttest.GethV3)
	e.start(t.Context())

	stale := gethNeed(agenttest.GethV2)
	e.grant("lease-stale", stale)

	op := e.awaitFinished("lease-stale")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted, "a superseded step costs nothing")
	assert.Empty(t, op.Error)

	step := stepOf(t, op, stale.ID)
	assert.Equal(t, protocol.StepSuperseded, step.Status)
	assert.Contains(t, step.Error, agenttest.GethV3)

	assert.Empty(t, e.disruptive())
	assert.Equal(t, agenttest.GethV1, e.host.Container("geth").Digest)
}

func TestDigestMismatchFailsWhenTheTagStillPointsThere(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	// The registry has geth-2, but the image never arrived locally.
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-unpulled", need)

	op := e.awaitFinished("lease-unpulled")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.False(t, op.Disrupted, "the digest is checked before anything stops")
	assert.Contains(t, op.Error, "although the tag still points at it")

	step := stepOf(t, op, need.ID)
	assert.Equal(t, protocol.StepFailed, step.Status)
	assert.Equal(t, op.Error, step.Error)

	assert.Empty(t, e.disruptive())
	assert.Equal(t, agenttest.GethV1, e.host.Container("geth").Digest)
	assert.Equal(t, protocol.StateRunning, e.host.Container("geth").State)
}

// A name-mapped container gets its group labels on recreate; a default-group one none.
func TestRecreateLabelsMappedContainers(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Containers[1].Group, sc.Containers[1].Kind = "", ""
	sc.Containers[3].Group = ""

	e := newEnv(t, sc)
	e.policy.Containers = map[string]plan.MappedContainer{
		"geth": {Group: agenttest.GroupExecution, Kind: protocol.KindEL},
	}
	e.policy.DefaultGroup.Roll = true
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull("ethpandaops/dora:latest", "sha256:dora-2")
	e.start(t.Context())

	e.grant("lease-mapped",
		agenttest.ImageNeedFor("dora", "ethpandaops/dora:latest", "sha256:dora-1", "sha256:dora-2"),
		gethNeed(agenttest.GethV2))

	op := e.awaitFinished("lease-mapped")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	geth := e.host.Container("geth")
	assert.Equal(t, agenttest.GethV2, geth.Digest)
	assert.Equal(t, agenttest.GroupExecution, geth.Group, "the recreate set the group label")
	assert.Equal(t, protocol.KindEL, geth.Kind)

	dora := e.host.Container("dora")
	assert.Equal(t, "sha256:dora-2", dora.Digest)
	assert.Empty(t, dora.Group, "the default group is no label")

	// Ordered by the mapped kind: the execution client first.
	assert.Equal(t, []string{
		"recreate geth " + agenttest.GethV2 + " start=true image=" + agenttest.GethImage,
		"recreate dora sha256:dora-2 start=true",
		"start geth", "start teku", "start validator", "start dora",
	}, e.disruptive())
}

func TestRollOrder(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()

	e := newEnv(t, sc)
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)
	e.pull(agenttest.VCImage, agenttest.VCV2)
	e.start(t.Context())

	// Granted in an order that is not the order of execution.
	needs := []protocol.Need{vcNeed(), tekuNeed(), agenttest.PackagesNeed(), gethNeed(agenttest.GethV2)}
	e.grant("lease-all", needs...)

	op := e.awaitFinished("lease-all")
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
	}, e.disruptive())

	assert.Equal(t, "sha256:dora-1", e.host.Container("dora").Digest)
	assert.Eventually(t, func() bool { return len(e.audit.got()) == len(needs) }, agenttest.AwaitTimeout, awaitPoll)
	assert.Equal(t, []string{
		needs[0].ID + " done", needs[1].ID + " done", needs[2].ID + " done", needs[3].ID + " done",
	}, e.audit.got(), "every need goes to the audit log")

	e.assertPhases("lease-all", protocol.PhaseApplying, protocol.PhaseStarting,
		protocol.PhaseVerifying, protocol.PhaseDone)
}

// What a container is recreated as comes from the host's files, never from the lease.
func TestRecreateTakesTheTargetFromTheHostsFiles(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.policy.Groups[agenttest.GroupExecution] = plan.Group{Overrides: []string{protocol.OverrideImage}}
	e.files.set(func(o *plan.Overrides) {
		o.Entries = map[string]plan.Entry{"geth": {Image: agenttest.FixImage, By: "alice", Reason: "test"}}
	})
	e.pull(agenttest.FixImage, agenttest.FixV1)

	need := overrideNeed(t, e)

	// The lease names another image; the container gets the file's.
	need.Override.Image = "attacker/evil:latest"
	need.Override.Flags = true

	e.start(t.Context())
	e.grant("lease-override", need)

	op := e.awaitFinished("lease-override")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, need.ID).Status)

	assert.Equal(t, "recreate geth "+agenttest.FixV1+" start=true image="+agenttest.FixImage,
		e.disruptive()[0])
	assert.Equal(t, agenttest.FixImage, e.host.Container("geth").Image)
}

// A file changed after validation wins: the target is read right before the recreate.
func TestRecreateRereadsTheFilesRightBefore(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()

	e := newEnv(t, sc)
	e.policy.Groups[agenttest.GroupExecution] = plan.Group{Overrides: []string{protocol.OverrideImage}}
	e.files.set(func(o *plan.Overrides) {
		o.Entries = map[string]plan.Entry{"geth": {Image: agenttest.FixImage, By: "alice", Reason: "test"}}
	})
	e.pull(agenttest.FixImage, agenttest.FixV1)

	need := overrideNeed(t, e)
	entered, release := e.block("upgrade ")

	e.start(t.Context())
	e.grant("lease-override", agenttest.PackagesNeed(), need)

	agenttest.AwaitParked(t, entered, "upgrade")
	e.files.set(func(o *plan.Overrides) { o.Entries = nil })
	release()

	op := e.awaitFinished("lease-override")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepSuperseded, stepOf(t, op, need.ID).Status)
	assert.Equal(t, "container geth already runs what agent.yaml and the overrides file say",
		stepOf(t, op, need.ID).Error)
	assert.NotContains(t, strings.Join(e.disruptive(), "\n"), "recreate")
	assert.Equal(t, agenttest.GethImage, e.host.Container("geth").Image)
}

// overrideNeed is geth's override need with the override image local.
func overrideNeed(t *testing.T, e *env) protocol.Need {
	t.Helper()

	list, err := e.host.Containers(t.Context())
	require.NoError(t, err)

	files := e.files.Load()
	view := plan.Classify(e.policy, &plan.Facts{Containers: list, Fresh: true}, &files,
		map[string]protocol.RegistryCheck{
			imageref.Normalize(agenttest.FixImage): {Digest: agenttest.FixV1, Pulled: true},
		}, e.now())

	for _, n := range plan.Needs(e.policy, &view, nil) {
		if n.Kind == protocol.NeedOverride {
			return n
		}
	}

	require.FailNow(t, "no override need")

	return protocol.Need{}
}

// A digest mismatch clears the disrupted mark only when its call set it.
func TestDigestMismatchKeepsAnEarlierMark(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, "sha256:teku-3")
	e.start(t.Context())

	geth, teku := gethNeed(agenttest.GethV2), tekuNeed()
	e.grant("lease-1", geth, teku)

	op := e.awaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, geth.ID).Status)
	assert.Equal(t, protocol.StepSuperseded, stepOf(t, op, teku.ID).Status)
	e.assertPhases("lease-1", protocol.PhaseApplying, protocol.PhaseStarting,
		protocol.PhaseVerifying, protocol.PhaseDone)
}

// The step fails when the containers cannot be listed right before the recreate.
func TestRecreateWithoutAContainerList(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()

	e := newEnv(t, sc)
	e.pull(agenttest.GethImage, agenttest.GethV2)

	entered, release := e.block("upgrade ")

	e.start(t.Context())

	geth := gethNeed(agenttest.GethV2)
	e.grant("lease-1", agenttest.PackagesNeed(), geth)

	agenttest.AwaitParked(t, entered, "upgrade")
	e.host.Fail("observe", errors.New("docker is gone"))
	release()

	op := e.awaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Equal(t, "cannot list containers before recreating geth: agenttest: observe: docker is gone",
		stepOf(t, op, geth.ID).Error)
	assert.NotContains(t, strings.Join(e.disruptive(), "\n"), "recreate")
}

// A digest mismatch the registry cannot explain fails the step.
func TestDigestMismatchWithoutTheRegistry(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.host.Fail("resolve:"+agenttest.GethImage, errors.New("registry is down"))
	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-1", need)

	op := e.awaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.False(t, op.Disrupted)

	step := stepOf(t, op, need.ID)
	assert.Equal(t, protocol.StepFailed, step.Status)
	assert.Equal(t, "the local image of "+agenttest.GethImage+" is not the granted digest "+agenttest.GethV2+
		" and the registry could not be asked: agenttest: resolve "+agenttest.GethImage+": registry is down",
		step.Error)
}
