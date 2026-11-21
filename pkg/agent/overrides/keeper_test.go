package overrides_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// An override written into the file by hand is applied under a lease, the
// container then follows the override's tag, and it is put back, command
// line included, once the entry is gone. The lease names the need; what the
// container becomes comes from the file.
func TestOverrideByHand(t *testing.T) {
	t.Parallel()

	e := agenttest.NewOverrideEnv(t)
	e.Ctrl.AutoRelease = true
	e.Start()
	e.Ctrl.AwaitSyncs(2)

	e.WriteOverrides("overrides:\n  geth:\n    image: " + agenttest.FixImage + "\n    flags:\n" +
		"      add: [\"--pprof\"]\n      remove: [\"--maxpeers\"]\n    by: sam\n    reason: sync\n")

	need := agenttest.AwaitOverrideNeed(t, e, "geth", false)
	assert.True(t, need.Local)
	assert.Equal(t, &protocol.OverrideNeed{
		Container: "geth", Image: agenttest.FixImage, FromImage: agenttest.GethImage, FromDigest: agenttest.GethV1,
		ToDigest: agenttest.FixV1, ToRevision: "rev-fix", Flags: true,
	}, need.Override)

	req := e.Ctrl.Await("the override is reported", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Override != nil
	})
	geth := agenttest.ContainerOf(t, &req, "geth")
	assert.False(t, geth.Override.Applied)
	assert.Equal(t, "sam", geth.Override.By)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, geth.Command)
	assert.Empty(t, agenttest.ContainerOf(t, &req, "dora").Command, "an unmanaged container's command line is not reported")

	e.Ctrl.Grant(e.Lease("lease-override", need))

	op := e.Ctrl.AwaitFinished("lease-override")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Contains(t, agenttest.Disruptive(e.Host.Calls()),
		"recreate geth "+agenttest.FixV1+" start=true image="+agenttest.FixImage+" command=--http --pprof")

	req = e.Ctrl.Await("the override is applied", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Override != nil && c.Override.Applied && len(req.Needs) == 0
	})
	geth = agenttest.ContainerOf(t, &req, "geth")
	assert.Equal(t, agenttest.FixImage, geth.Image)
	assert.Equal(t, agenttest.FixV1, geth.Digest)
	assert.Equal(t, agenttest.GethImage, geth.ConfiguredImage)
	assert.Equal(t, []string{"--http", "--pprof"}, geth.Command)

	// The override's tag moves: an ordinary image need, like any tag.
	e.Host.SetRegistry(agenttest.FixImage, agenttest.FixV2, "")

	follow := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.FixV2))
	assert.Equal(t, protocol.NeedImage, follow[0].Kind)
	assert.Equal(t, agenttest.FixImage, follow[0].Image.Image)

	// A reset: the file holds nothing but comments.
	e.WriteOverrides("# nothing\noverrides:\n")

	back := agenttest.AwaitOverrideNeed(t, e, "geth", true)
	assert.Equal(t, agenttest.GethImage, back.Override.Image, "what the container was created with")
	assert.True(t, back.Override.Flags)

	req = e.Ctrl.Await("restoring is reported", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Override != nil && c.Override.Restoring
	})
	assert.Equal(t, agenttest.FixImage, agenttest.ContainerOf(t, &req, "geth").Image)

	e.Ctrl.Grant(e.Lease("lease-restore", back))

	op = e.Ctrl.AwaitFinished("lease-restore")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	req = e.Ctrl.Await("the container is back", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Override == nil && len(req.Needs) == 0
	})
	geth = agenttest.ContainerOf(t, &req, "geth")
	assert.Equal(t, agenttest.GethImage, geth.Image)
	assert.Equal(t, agenttest.GethV1, geth.Digest)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, geth.Command)
	assert.Nil(t, e.Host.Container("geth").Base, "the labels are gone with the override")

	assert.Equal(t, []string{
		"override.edit accepted", "override.apply done", "override.edit accepted", "override.restore done",
	}, agenttest.AuditActions(e.AuditRecords()))

	apply := e.AuditRecords()[1]
	assert.Equal(t, "lease-override", apply.Lease)
	assert.Equal(t, "geth", apply.Container)
}

// An override ends by itself: the agent takes the entry out of the file,
// leaves a comment line, and puts the container back under a lease.
func TestOverrideExpires(t *testing.T) {
	t.Parallel()

	e := agenttest.NewOverrideEnv(t)
	e.Ctrl.AutoRelease = true
	e.Start()
	e.Ctrl.AwaitSyncs(2)

	until := e.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	e.WriteOverrides("# mine\noverrides:\n  geth:\n    image: " + agenttest.FixImage + "\n    by: sam\n" +
		"    reason: sync\n    until: " + until + "\n  teku:\n    flags: {add: [\"--x\"]}\n")

	need := agenttest.AwaitOverrideNeed(t, e, "geth", false)
	teku := agenttest.AwaitOverrideNeed(t, e, "teku", false)
	e.Ctrl.Grant(e.Lease("lease-1", need, teku))
	require.Equal(t, protocol.PhaseDone, e.Ctrl.AwaitFinished("lease-1").Phase)

	e.ClockOffset.Add(int64(2 * time.Hour))

	back := agenttest.AwaitOverrideNeed(t, e, "geth", true)

	raw, err := os.ReadFile(e.Cfg.OverridesFile)
	require.NoError(t, err)

	content := string(raw)
	assert.NotContains(t, content, "\n  geth:")
	assert.Contains(t, content, "# [geth] ")
	assert.Contains(t, content, "upgradoor: expired; was image "+agenttest.FixImage+" by sam")
	assert.True(t, strings.HasPrefix(content, "# mine\noverrides:\n"))
	assert.Contains(t, content, "  teku:\n    flags: {add: [\"--x\"]}\n", "the other entry is as it was written")

	e.Ctrl.Grant(e.Lease("lease-2", back))
	require.Equal(t, protocol.PhaseDone, e.Ctrl.AwaitFinished("lease-2").Phase)

	req := e.Ctrl.Await("geth is back, teku stays", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Override == nil && len(req.Needs) == 0
	})
	assert.Equal(t, agenttest.GethImage, agenttest.ContainerOf(t, &req, "geth").Image)
	assert.True(t, agenttest.ContainerOf(t, &req, "teku").Override.Applied)
	assert.Contains(t, agenttest.AuditActions(e.AuditRecords()), "override.expire accepted")
}

// What cannot be applied changes nothing and is said: an image that does not
// exist, a file that does not parse, a hold.
func TestOverrideThatCannotBeApplied(t *testing.T) {
	t.Parallel()

	e := agenttest.NewOverrideEnv(t)
	e.Start()
	e.Ctrl.AwaitSyncs(2)

	// An image the registry does not have: no need, ever.
	e.WriteOverrides("overrides:\n  geth:\n    image: ethpandaops/geth:nope\n")

	req := e.Ctrl.Await("the override is reported", func(req *protocol.SyncRequest) bool {
		return agenttest.ContainerOf(t, req, "geth").Override != nil
	})
	assert.False(t, agenttest.ContainerOf(t, &req, "geth").Override.Applied)
	e.Ctrl.AwaitSyncs(e.Ctrl.SyncCount() + 3)
	assert.Empty(t, e.Ctrl.AllSyncs()[e.Ctrl.SyncCount()-1].Needs)

	// A broken file applies nothing and takes nothing back.
	e.WriteOverrides("overrides: [geth\n")
	e.Ctrl.AwaitSyncs(e.Ctrl.SyncCount() + 3)

	last := e.Ctrl.AllSyncs()[e.Ctrl.SyncCount()-1]
	assert.Empty(t, last.Needs)
	assert.Nil(t, agenttest.ContainerOf(t, &last, "geth").Override)

	// A lease for a need the file no longer asks for is superseded.
	e.WriteOverrides("overrides:\n  geth:\n    image: " + agenttest.FixImage + "\n")
	need := agenttest.AwaitOverrideNeed(t, e, "geth", false)

	e.WriteOverrides("overrides:\n  geth:\n    image: " + agenttest.FixImage + "\n    flags: {add: [\"--y\"]}\n")
	agenttest.AwaitNeeds(t, e, awaitOverrideNeedOtherThan(t, e, need.ID).ID)

	e.Ctrl.Grant(e.Lease("lease-stale", need))

	op := e.Ctrl.AwaitFinished("lease-stale")
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepSuperseded, agenttest.StepOf(t, op, need.ID).Status)
	assert.Contains(t, agenttest.StepOf(t, op, need.ID).Error, "asks for something else now")
	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()))
}

func awaitOverrideNeedOtherThan(t *testing.T, e *agenttest.Env, id string) protocol.Need {
	t.Helper()

	var found protocol.Need

	e.Ctrl.Await("another override need", func(req *protocol.SyncRequest) bool {
		for _, n := range req.Needs {
			if n.Kind == protocol.NeedOverride && n.ID != id && len(n.Unpulled) == 0 {
				found = n

				return true
			}
		}

		return false
	})

	return found
}
