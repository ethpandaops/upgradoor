package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	fixImage = "ethpandaops/geth:fix"
	fixV1    = "sha256:fix-1"
	fixV2    = "sha256:fix-2"
)

// overrideEnv is an agent on a host whose geth has a command line, with the
// override image in the registry.
func overrideEnv(t *testing.T) *env {
	t.Helper()

	sc := baseScenario()
	sc.Containers[1].Command = []string{"--http", "--maxpeers=50"}

	e := newEnv(t, sc)
	e.host.SetRegistry(fixImage, fixV1, "rev-fix")

	return e
}

func (e *env) writeOverrides(content string) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(e.cfg.OverridesFile, []byte(content), 0o600))
}

// auditActions reads the actions of the host's audit log, oldest first.
func (e *env) auditRecords() []auditlog.Record {
	e.t.Helper()

	f, err := os.Open(e.cfg.AuditLog.File)
	if err != nil {
		return nil
	}

	defer func() { _ = f.Close() }()

	var out []auditlog.Record

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec auditlog.Record
		require.NoError(e.t, json.Unmarshal(sc.Bytes(), &rec))
		out = append(out, rec)
	}

	return out
}

func auditActions(records []auditlog.Record) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.Action+" "+r.Result)
	}

	return out
}

// awaitOverrideNeed waits for the override need of a container.
func awaitOverrideNeed(t *testing.T, e *env, container string, restore bool) protocol.Need {
	t.Helper()

	var found protocol.Need

	e.ctrl.await("an override need for "+container, func(req *protocol.SyncRequest) bool {
		for _, n := range req.Needs {
			if n.Kind == protocol.NeedOverride && n.Override.Container == container &&
				n.Override.Restore == restore {
				found = n

				return true
			}
		}

		return false
	})

	return found
}

// An override written into the file by hand is applied under a lease, the
// container then follows the override's tag, and it is put back, command
// line included, once the entry is gone. The lease names the need; what the
// container becomes comes from the file.
func TestOverrideByHand(t *testing.T) {
	e := overrideEnv(t)
	e.ctrl.autoRelease = true
	e.start()
	e.ctrl.awaitSyncs(2)

	e.writeOverrides("overrides:\n  geth:\n    image: " + fixImage + "\n    flags:\n" +
		"      add: [\"--pprof\"]\n      remove: [\"--maxpeers\"]\n    by: sam\n    reason: sync\n")

	need := awaitOverrideNeed(t, e, "geth", false)
	assert.True(t, need.Local)
	assert.Equal(t, &protocol.OverrideNeed{
		Container: "geth", Image: fixImage, FromImage: gethImage, FromDigest: gethV1,
		ToDigest: fixV1, ToRevision: "rev-fix", Flags: true,
	}, need.Override)

	req := e.ctrl.await("the override is reported", func(req *protocol.SyncRequest) bool {
		return containerOf(t, req, "geth").Override != nil
	})
	geth := containerOf(t, &req, "geth")
	assert.False(t, geth.Override.Applied)
	assert.Equal(t, "sam", geth.Override.By)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, geth.Command)
	assert.Empty(t, containerOf(t, &req, "dora").Command, "an unmanaged container's command line is not reported")

	e.ctrl.grant(e.lease("lease-override", need))

	op := e.ctrl.awaitFinished("lease-override")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Contains(t, disruptive(e.host.Calls()),
		"recreate geth "+fixV1+" start=true image="+fixImage+" command=--http --pprof")

	req = e.ctrl.await("the override is applied", func(req *protocol.SyncRequest) bool {
		c := containerOf(t, req, "geth")

		return c.Override != nil && c.Override.Applied && len(req.Needs) == 0
	})
	geth = containerOf(t, &req, "geth")
	assert.Equal(t, fixImage, geth.Image)
	assert.Equal(t, fixV1, geth.Digest)
	assert.Equal(t, "docker.io/"+gethImage, geth.ConfiguredImage)
	assert.Equal(t, []string{"--http", "--pprof"}, geth.Command)

	// The override's tag moves: an ordinary image need, like any tag.
	e.host.SetRegistry(fixImage, fixV2, "")

	follow := awaitNeeds(t, e, protocol.ImageNeedID("geth", fixV2))
	assert.Equal(t, protocol.NeedImage, follow[0].Kind)
	assert.Equal(t, fixImage, follow[0].Image.Image)

	// A reset: the file holds nothing but comments.
	e.writeOverrides("# nothing\noverrides:\n")

	back := awaitOverrideNeed(t, e, "geth", true)
	assert.Equal(t, "docker.io/"+gethImage, back.Override.Image, "the reference of the configuration")
	assert.True(t, back.Override.Flags)

	req = e.ctrl.await("restoring is reported", func(req *protocol.SyncRequest) bool {
		c := containerOf(t, req, "geth")

		return c.Override != nil && c.Override.Restoring
	})
	assert.Equal(t, fixImage, containerOf(t, &req, "geth").Image)

	e.ctrl.grant(e.lease("lease-restore", back))

	op = e.ctrl.awaitFinished("lease-restore")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	req = e.ctrl.await("the container is back", func(req *protocol.SyncRequest) bool {
		return containerOf(t, req, "geth").Override == nil && len(req.Needs) == 0
	})
	geth = containerOf(t, &req, "geth")
	assert.Equal(t, "docker.io/"+gethImage, geth.Image)
	assert.Equal(t, gethV1, geth.Digest)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, geth.Command)
	assert.Nil(t, e.host.Container("geth").Base, "the labels are gone with the override")

	assert.Equal(t, []string{
		"override.edit accepted", "override.apply done", "override.edit accepted", "override.restore done",
	}, auditActions(e.auditRecords()))

	apply := e.auditRecords()[1]
	assert.Equal(t, "lease-override", apply.Lease)
	assert.Equal(t, "geth", apply.Container)
}

// An override ends by itself: the agent takes the entry out of the file,
// leaves a comment line, and puts the container back under a lease.
func TestOverrideExpires(t *testing.T) {
	e := overrideEnv(t)
	e.ctrl.autoRelease = true
	e.start()
	e.ctrl.awaitSyncs(2)

	until := e.now().Add(time.Hour).UTC().Format(time.RFC3339)
	e.writeOverrides("# mine\noverrides:\n  geth:\n    image: " + fixImage + "\n    by: sam\n" +
		"    reason: sync\n    until: " + until + "\n  teku:\n    flags: {add: [\"--x\"]}\n")

	need := awaitOverrideNeed(t, e, "geth", false)
	teku := awaitOverrideNeed(t, e, "teku", false)
	e.ctrl.grant(e.lease("lease-1", need, teku))
	require.Equal(t, protocol.PhaseDone, e.ctrl.awaitFinished("lease-1").Phase)

	e.clockOffset.Add(int64(2 * time.Hour))

	back := awaitOverrideNeed(t, e, "geth", true)

	raw, err := os.ReadFile(e.cfg.OverridesFile)
	require.NoError(t, err)

	content := string(raw)
	assert.NotContains(t, content, "\n  geth:")
	assert.Contains(t, content, "# [geth] ")
	assert.Contains(t, content, "upgradoor: expired; was image "+fixImage+" by sam")
	assert.True(t, strings.HasPrefix(content, "# mine\noverrides:\n"))
	assert.Contains(t, content, "  teku:\n    flags: {add: [\"--x\"]}\n", "the other entry is as it was written")

	e.ctrl.grant(e.lease("lease-2", back))
	require.Equal(t, protocol.PhaseDone, e.ctrl.awaitFinished("lease-2").Phase)

	req := e.ctrl.await("geth is back, teku stays", func(req *protocol.SyncRequest) bool {
		return containerOf(t, req, "geth").Override == nil && len(req.Needs) == 0
	})
	assert.Equal(t, "docker.io/"+gethImage, containerOf(t, &req, "geth").Image)
	assert.True(t, containerOf(t, &req, "teku").Override.Applied)
	assert.Contains(t, auditActions(e.auditRecords()), "override.expire accepted")
}

// What cannot be applied changes nothing and is said: an image that does not
// exist, a file that does not parse, a hold.
func TestOverrideThatCannotBeApplied(t *testing.T) {
	e := overrideEnv(t)
	e.start()
	e.ctrl.awaitSyncs(2)

	// An image the registry does not have: no need, ever.
	e.writeOverrides("overrides:\n  geth:\n    image: ethpandaops/geth:nope\n")

	req := e.ctrl.await("the override is reported", func(req *protocol.SyncRequest) bool {
		return containerOf(t, req, "geth").Override != nil
	})
	assert.False(t, containerOf(t, &req, "geth").Override.Applied)
	e.ctrl.awaitSyncs(e.ctrl.syncCount() + 3)
	assert.Empty(t, e.ctrl.allSyncs()[e.ctrl.syncCount()-1].Needs)

	// A broken file applies nothing and takes nothing back.
	e.writeOverrides("overrides: [geth\n")
	e.ctrl.awaitSyncs(e.ctrl.syncCount() + 3)

	last := e.ctrl.allSyncs()[e.ctrl.syncCount()-1]
	assert.Empty(t, last.Needs)
	assert.Nil(t, containerOf(t, &last, "geth").Override)

	// A lease for a need the file no longer asks for is superseded.
	e.writeOverrides("overrides:\n  geth:\n    image: " + fixImage + "\n")
	need := awaitOverrideNeed(t, e, "geth", false)

	e.writeOverrides("overrides:\n  geth:\n    image: " + fixImage + "\n    flags: {add: [\"--y\"]}\n")
	awaitNeeds(t, e, awaitOverrideNeedOtherThan(t, e, need.ID).ID)

	e.ctrl.grant(e.lease("lease-stale", need))

	op := e.ctrl.awaitFinished("lease-stale")
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepSuperseded, stepOf(t, op, need.ID).Status)
	assert.Contains(t, stepOf(t, op, need.ID).Error, "asks for something else now")
	assert.Empty(t, disruptive(e.host.Calls()))
}

func awaitOverrideNeedOtherThan(t *testing.T, e *env, id string) protocol.Need {
	t.Helper()

	var found protocol.Need

	e.ctrl.await("another override need", func(req *protocol.SyncRequest) bool {
		for _, n := range req.Needs {
			if n.Kind == protocol.NeedOverride && n.ID != id {
				found = n

				return true
			}
		}

		return false
	})

	return found
}
