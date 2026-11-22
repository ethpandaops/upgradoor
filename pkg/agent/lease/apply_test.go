package lease

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestPullSupersedesWhenTheTagMovedOn(t *testing.T) {
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

func TestLeasePullsAnImageThatIsNotLocal(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.pull(agenttest.TekuImage, agenttest.TekuV2)
	e.start(t.Context())

	e.grant("lease-1", gethNeed(agenttest.GethV2), tekuNeed())

	op := e.awaitFinished("lease-1")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest)
	assert.Equal(t, agenttest.TekuV2, e.host.Container("teku").Digest)
	e.assertPhases("lease-1", protocol.PhasePulling, protocol.PhaseApplying, protocol.PhaseStarting,
		protocol.PhaseVerifying, protocol.PhaseDone)

	// Pulled before anything stopped, and only what was not local.
	calls := e.host.Calls()
	assert.Less(t, slices.Index(calls, "pull "+agenttest.GethImage),
		slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "recreate ") }))
	assert.Equal(t, []string{"pulled " + agenttest.GethImage}, e.host.PullNotes())
}

// A pull that fails defers every step of the lease: nothing is begun.
func TestFailedPullDefersTheLease(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.pull(agenttest.TekuImage, agenttest.TekuV2)
	e.host.Fail("pull:"+agenttest.GethImage, errors.New("registry is down"))
	e.start(t.Context())

	geth, teku := gethNeed(agenttest.GethV2), tekuNeed()
	e.grant("lease-1", teku, geth)

	op := e.awaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Empty(t, op.Error)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepDeferred, s.Status, s.NeedID)
		assert.Equal(t, "pull "+agenttest.GethImage+": agenttest: pull "+agenttest.GethImage+": registry is down", s.Error)
	}

	assert.Empty(t, e.disruptive())
	assert.Equal(t, []string{"failed " + agenttest.GethImage}, e.host.PullNotes())
	e.assertPhases("lease-1", protocol.PhasePulling, protocol.PhaseDone)
}

// A recreate keeps the labels; an unlabelled container under defaultPolicy
// roll is rolled after the labelled kinds.
func TestRecreateKeepsLabels(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Containers[3].Kind = ""

	e := newEnv(t, sc)
	e.policy.DefaultPolicy = plan.DefaultRoll
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull("ethpandaops/dora:latest", "sha256:dora-2")
	e.start(t.Context())

	e.grant("lease-labels",
		agenttest.ImageNeedFor("dora", "ethpandaops/dora:latest", "sha256:dora-1", "sha256:dora-2"),
		gethNeed(agenttest.GethV2))

	op := e.awaitFinished("lease-labels")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	geth := e.host.Container("geth")
	assert.Equal(t, agenttest.GethV2, geth.Digest)
	assert.Equal(t, protocol.KindEL, geth.Kind)
	assert.Equal(t, agenttest.TeamGeth, geth.Team)

	dora := e.host.Container("dora")
	assert.Equal(t, "sha256:dora-2", dora.Digest)
	assert.Empty(t, dora.Kind, "no labels are added")

	// The execution client first, the unlabelled container last.
	assert.Equal(t, []string{
		"recreate geth " + agenttest.GethV2 + " start=true",
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

	e.assertPhases("lease-all", protocol.PhasePulling, protocol.PhaseApplying, protocol.PhaseStarting,
		protocol.PhaseVerifying, protocol.PhaseDone)
}

// What a container is recreated as comes from the host's files, never from the lease.
// A package upgrade that restarted no container is not a disruption.
func TestPackageUpgradeWithoutRestartsIsNotDisruptive(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = []protocol.Upgradable{
		{Name: "containerd.io", Arch: "amd64", Current: "1.7", Candidate: "1.7.1"},
	}

	pkgs := []protocol.PackageChange{{Name: "containerd.io", From: "1.7", To: "1.7.1"}}
	need := protocol.Need{
		ID: protocol.PackagesNeedID(pkgs), Kind: protocol.NeedPackages,
		Packages: &protocol.PackagesNeed{Packages: pkgs},
	}

	e := newEnv(t, sc)
	e.start(t.Context())
	e.grant("lease-1", need)

	op := e.awaitFinished("lease-1")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, need.ID).Status)
	e.assertPhases("lease-1", protocol.PhaseApplying, protocol.PhaseDone)
}

func TestRecreateTakesTheTargetFromTheHostsFiles(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
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

// A recreate whose tag moved on is superseded; the others of the lease are served.
func TestSupersededPullKeepsTheOtherRecreates(t *testing.T) {
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
	e.assertPhases("lease-1", protocol.PhasePulling, protocol.PhaseApplying, protocol.PhaseStarting,
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

// A digest mismatch without a known digest of the tag fails the step.

// movedHost plays a recreate of geth that found the tag moved after the stop
// and whose rollback could not start the old container again.
type movedHost struct {
	*agenttest.Host
}

func (h *movedHost) Recreate(
	ctx context.Context, name, digest string, timeout time.Duration, start bool, change *host.Change,
) error {
	if name != "geth" {
		return h.Host.Recreate(ctx, name, digest, timeout, start, change)
	}

	if err := h.Stop(ctx, name, timeout); err != nil {
		return err
	}

	return fmt.Errorf("recreate container geth: %w (rollback incomplete: start old container: boom)",
		host.ErrImageMoved)
}

// A tag that moved after the stop supersedes the step, but the container was
// disrupted: it is brought back and watched.
func TestImageMovedAfterTheStopIsADisruption(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.machine = &movedHost{Host: e.host}
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.start(t.Context())

	geth := gethNeed(agenttest.GethV2)
	e.grant("lease-1", geth)

	op := e.awaitFinished("lease-1")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted, "geth was stopped")
	assert.Equal(t, protocol.StepSuperseded, stepOf(t, op, geth.ID).Status)

	assert.Equal(t, []string{"stop geth", "start geth", "start teku", "start validator"}, e.disruptive())
	e.assertAllRunning()
	e.assertPhases("lease-1", protocol.PhasePulling, protocol.PhaseApplying, protocol.PhaseStarting,
		protocol.PhaseVerifying, protocol.PhaseDone)
}
