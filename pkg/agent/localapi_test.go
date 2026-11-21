package agent

import (
	"errors"
	"os"
	"os/user"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// awaitLocal polls the socket's status until pred holds.
func awaitLocal(t *testing.T, c local.Client, what string, pred func(st *local.Status) bool) *local.Status {
	t.Helper()

	var last *local.Status

	require.Eventually(t, func() bool {
		st, err := c.Status(t.Context())
		if err != nil {
			return false
		}

		last = st

		return pred(st)
	}, agenttest.AwaitTimeout, 2*time.Millisecond, "the local status never showed: %s", what)

	return last
}

func localContainer(t *testing.T, st *local.Status, name string) local.Container {
	t.Helper()

	i := slices.IndexFunc(st.Containers, func(c local.Container) bool { return c.Name == name })
	require.GreaterOrEqual(t, i, 0, "no container %s in the local status", name)

	return st.Containers[i]
}

// rollOf is the roll of that ID in a status, or nil.
func rollOf(st *local.Status, id string) *local.Roll {
	for i := range st.Rolls {
		if st.Rolls[i].ID == id {
			return &st.Rolls[i]
		}
	}

	return nil
}

func me(t *testing.T) string {
	t.Helper()

	u, err := user.Current()
	require.NoError(t, err)

	return u.Username
}

// An override set through the socket is written for the asking user and served.
func TestLocalOverride(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	e.start()

	c := local.NewClient(e.cfg.Local.Socket.Path)

	st := awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })
	assert.Equal(t, agenttest.HostName, st.Host)
	assert.Equal(t, e.cfg.Local.OverridesFile, st.OverridesFile)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, localContainer(t, st, "geth").Command)

	info, err := os.Stat(e.cfg.Local.Socket.Path)
	require.NoError(t, err)
	assert.Zero(t, info.Mode().Perm()&0o007, "the socket is not for everybody")

	_, err = c.InitOverrides(t.Context())
	require.NoError(t, err)

	until := e.now().Add(time.Hour).UTC().Truncate(time.Second)

	answer, err := c.Override(t.Context(), &local.OverrideRequest{
		Container: "geth", Image: agenttest.FixImage, AddFlags: []string{"--pprof"}, Until: &until, Reason: "sync",
	})
	require.NoError(t, err)
	assert.Contains(t, answer.Message, "image "+agenttest.FixImage+", flags +--pprof")

	content := e.readOverrides()
	assert.True(t, strings.HasPrefix(content, "# >>> upgradoor generated block"))
	assert.Contains(t, content, "#   upgradoor override set <container>")
	assert.Contains(t, content, "deployed:\n  \"geth\":\n    image: \""+agenttest.GethImage+"\"\n"+
		"    command: [\"--http\", \"--maxpeers=50\"]\n")
	assert.Contains(t, content, "local:"+me(t)+": set image "+agenttest.FixImage+", flags +--pprof; was nothing\n")
	assert.Contains(t, content, "    by: \""+me(t)+"\"\n    reason: \"sync\"\n    until: "+until.Format(time.RFC3339)+"\n")

	need := awaitOverrideNeed(t, e, "geth", false)
	e.ctrl.Grant(e.lease("lease-local", need))
	require.Equal(t, protocol.PhaseDone, e.ctrl.AwaitFinished("lease-local").Phase)

	st = awaitLocal(t, c, "the override applied", func(st *local.Status) bool {
		o := localContainer(t, st, "geth").Override

		return o != nil && o.Applied
	})
	geth := localContainer(t, st, "geth")
	assert.Equal(t, []string{"--http", "--maxpeers=50", "--pprof"}, geth.Command)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, geth.BaseCommand, "what the override changed is told apart")

	// The host's own rules refuse, and nothing changes.
	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "dora", Image: agenttest.FixImage})
	require.ErrorContains(t, err, "container dora is not managed")

	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Image: "ethpandaops/geth@sha256:abc"})
	require.Error(t, err)

	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Clear: true, Reason: "done"})
	require.NoError(t, err)
	awaitOverrideNeed(t, e, "geth", true)

	records := e.auditRecords()
	assert.Equal(t, []string{
		"overrides.init accepted", "override.set accepted", "override.apply done",
		"override.set refused", "override.set refused", "override.clear accepted",
	}, auditActions(records))
	assert.Equal(t, "local:"+me(t), records[1].User)
	assert.Equal(t, "local", records[1].Source)
	assert.Equal(t, "nothing", records[1].Before)
	assert.Equal(t, "image "+agenttest.FixImage+", flags +--pprof", records[1].After)
}

func TestLocalClearOfNothing(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	e.start()

	c := local.NewClient(e.cfg.Local.Socket.Path)
	awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })

	_, err := c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Image: agenttest.FixImage})
	require.NoError(t, err)

	for range 2 {
		_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Clear: true})
		require.NoError(t, err)
	}

	records := e.auditRecords()
	assert.Equal(t, []string{"override.set accepted", "override.clear accepted", "override.clear accepted"},
		auditActions(records))
	assert.Empty(t, records[1].Error)
	assert.Equal(t, "there was no override", records[2].Error)
}

// A refused override records what was asked for.
func TestLocalRefusalRecordsTheAttempt(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	e.start()

	c := local.NewClient(e.cfg.Local.Socket.Path)
	awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })

	_, err := c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Image: "ethpandaops/geth@sha256:abc"})
	require.Error(t, err)

	records := e.auditRecords()
	require.Len(t, records, 1)
	assert.Equal(t, "override.set refused", records[0].Action+" "+records[0].Result)
	assert.Equal(t, "nothing", records[0].Before)
	assert.Equal(t, "image ethpandaops/geth@sha256:abc", records[0].After)
}

func TestLocalHoldThroughTheSocket(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.start()

	c := local.NewClient(e.cfg.Local.Socket.Path)
	awaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	_, err := c.Hold(t.Context(), &local.HoldRequest{Reason: "debugging"})
	require.NoError(t, err)

	req := e.ctrl.Await("the hold", func(req *protocol.SyncRequest) bool { return req.Hold.Local })
	assert.Equal(t, "debugging", req.Hold.LocalReason)
	assert.Contains(t, e.readOverrides(), "  local:\n    by: \""+me(t)+"\"\n", "the hold says who set it")
	assert.Empty(t, req.Needs, "a held host reports no needs")

	_, err = c.Roll(t.Context(), "geth")
	require.ErrorContains(t, err, "debugging")

	_, err = c.Hold(t.Context(), &local.HoldRequest{Release: true})
	require.NoError(t, err)
	awaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	assert.Equal(t, []string{"hold.set accepted", "roll.request refused", "hold.release accepted"},
		auditActions(e.auditRecords()))
}

// The status shows a write of the socket before the next sync; a release
// without a local hold is refused and names the controller's hold.
func TestLocalStatusFollowsTheWritesAtOnce(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	e.start()

	c := local.NewClient(e.cfg.Local.Socket.Path)
	awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })

	e.ctrl.Direct(protocol.Directive{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "maintenance"})
	e.ctrl.Await("the controller hold", func(req *protocol.SyncRequest) bool { return req.Hold.Controller })

	// From here on no sync is answered.
	var (
		entered = make(chan struct{}, 1)
		blocked = make(chan struct{})
	)

	t.Cleanup(func() { close(blocked) })
	e.ctrl.Set(func(c *agenttest.Controller) {
		c.Gate = func(*protocol.SyncRequest) {
			select {
			case entered <- struct{}{}:
			default:
			}

			<-blocked
		}
	})
	<-entered

	status := func() *local.Status {
		st, err := c.Status(t.Context())
		require.NoError(t, err)

		return st
	}

	_, err := c.Hold(t.Context(), &local.HoldRequest{Reason: "debugging"})
	require.NoError(t, err)
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging", Controller: true}, status().Hold)

	until := e.now().Add(time.Hour).UTC().Truncate(time.Second)
	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Image: agenttest.FixImage, Until: &until})
	require.NoError(t, err)

	o := localContainer(t, status(), "geth").Override
	require.NotNil(t, o)
	assert.Equal(t, agenttest.FixImage, o.Image)

	_, err = c.Hold(t.Context(), &local.HoldRequest{Release: true})
	require.NoError(t, err)
	assert.Equal(t, protocol.Hold{Controller: true}, status().Hold)

	_, err = c.Hold(t.Context(), &local.HoldRequest{Release: true})
	require.ErrorContains(t, err, "there is no local hold; the host is on controller hold")
	assert.Contains(t, err.Error(), "(maintenance), which is lifted on the controller")

	assert.Equal(t, []string{
		"hold.set accepted", "hold.set accepted", "override.set accepted", "hold.release accepted",
		"hold.release refused",
	}, auditActions(e.auditRecords()), "the controller's hold, then the socket's writes")
}

// A local roll is a need served under a lease, followed to the release.
func TestLocalRoll(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.ctrl.Set(func(c *agenttest.Controller) {
		c.Queue = &protocol.Queue{Position: 2, Reason: "budget 10% of 10%"}
	})
	e.start()

	c := local.NewClient(e.cfg.Local.Socket.Path)
	awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })

	_, err := c.Roll(t.Context(), "dora")
	require.ErrorContains(t, err, "not managed")

	answer, err := c.Roll(t.Context(), "geth")
	require.NoError(t, err)

	id := answer.Roll

	_, err = c.Roll(t.Context(), "geth")
	require.ErrorContains(t, err, "already under way")

	st := awaitLocal(t, c, "the roll queued", func(st *local.Status) bool {
		r := rollOf(st, id)

		return r != nil && r.State == local.RollQueued && r.Position == 2
	})
	assert.Equal(t, "budget 10% of 10%", rollOf(st, id).Detail)
	assert.Equal(t, "local:"+me(t), rollOf(st, id).RequestedBy)

	needs := awaitNeeds(t, e, protocol.LocalNeedID(id))
	assert.True(t, needs[0].Local)
	assert.Empty(t, needs[0].DirectiveID)
	assert.Equal(t, agenttest.GethV1, needs[0].Image.ToDigest, "onto the image the tag points at")

	e.ctrl.Grant(e.lease("lease-roll", needs...))

	op := e.ctrl.AwaitFinished("lease-roll")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Contains(t, agenttest.Disruptive(e.host.Calls()), "recreate geth "+agenttest.GethV1+" start=true")

	st = awaitLocal(t, c, "the roll done", func(st *local.Status) bool {
		r := rollOf(st, id)

		return r != nil && r.Final()
	})
	assert.Equal(t, local.RollDone, rollOf(st, id).State)
	assert.Equal(t, "lease-roll", rollOf(st, id).Lease)

	e.ctrl.Await("the need is closed", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})

	assert.Equal(t, []string{
		"roll.request refused", "roll.request accepted", "roll.request refused", "roll.result done",
	}, auditActions(e.auditRecords()))
}

// A local roll passes every state, each held until the test moves it on.
func TestLocalRollStates(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.cfg.Verify.Window = 300 * time.Millisecond
	e.ctrl.Set(func(c *agenttest.Controller) { c.AutoRelease = false })

	lookup := e.withhold(agenttest.GethImage)
	e.start()

	c := local.NewClient(e.cfg.Local.Socket.Path)
	awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })

	answer, err := c.Roll(t.Context(), "geth")
	require.NoError(t, err)

	id := answer.Roll
	state := func(want string) {
		t.Helper()

		awaitLocal(t, c, "the roll "+want, func(st *local.Status) bool {
			r := rollOf(st, id)

			return r != nil && r.State == want
		})
	}

	state(local.RollResolving)

	lookup.hold.Store(false)
	state(local.RollQueued)

	needs := awaitNeeds(t, e, protocol.LocalNeedID(id))

	// Docker does not answer: the lease is taken and not started.
	e.host.Fail("observe", errors.New("docker is restarting"))
	e.ctrl.Grant(e.lease("lease-roll", needs...))
	state(local.RollGranted)

	from := e.ctrl.SyncCount()
	e.ctrl.AwaitSyncs(3)

	for _, req := range e.ctrl.Syncs()[from:] {
		require.Nil(t, req.Operation, "a lease not started is no operation")
	}

	entered, release := e.host.BlockIn(t, "recreate geth")
	e.host.Fail("observe", nil)
	agenttest.AwaitParked(t, entered, "recreate geth")
	state(local.RollRecreating)

	release()
	state(local.RollRecreated)

	e.ctrl.AwaitFinished("lease-roll")
	state(local.RollVerified)

	e.ctrl.Set(func(c *agenttest.Controller) {
		c.Lease = nil
		c.Ended = &protocol.EndedLease{ID: "lease-roll", State: "released"}
	})
	state(local.RollDone)
}

// A deploy writes the block and tells Ansible what to leave to the agent; it
// wakes the loop only for a container off its target.
func TestLocalDeploy(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	a, ok := e.newAgent().(*agent)
	require.True(t, ok)

	api := localAPI{a}
	deploy := func(req *local.DeployRequest) *local.DeployResult {
		t.Helper()

		res, err := api.Deploy(t.Context(), "local:root", req)
		require.NoError(t, err)

		return res
	}
	woken := func() bool {
		select {
		case <-a.kick:
			return true
		default:
			return false
		}
	}
	gethCommand := []string{"--http", "--maxpeers=50"}
	managed := []string{"image", "command", "labels"}

	res := deploy(&local.DeployRequest{Container: "geth", Image: agenttest.GethImage, Command: gethCommand})
	assert.Equal(t, local.DeployResult{
		Container: "geth", Exists: true, Managed: true, Changed: true, Roll: local.DeployRollNone, Ignore: managed,
	}, *res)
	assert.False(t, woken(), "in line with the target")
	assert.Contains(t, e.readOverrides(), "  \"geth\":\n    image: \""+agenttest.GethImage+"\"\n"+
		"    command: [\"--http\", \"--maxpeers=50\"]\n")

	res = deploy(&local.DeployRequest{Container: "geth", Image: agenttest.FixImage, Command: gethCommand})
	assert.Equal(t, local.DeployResult{
		Container: "geth", Exists: true, Managed: true, Changed: true, Roll: local.DeployRollRequested,
		Ignore: managed,
	}, *res)
	assert.True(t, woken())

	// An override is kept by a deploy and dropped by a reset.
	_, _, err := a.files.SetEntry("geth", "dev", func(*plan.Entry) (*plan.Entry, error) {
		return &plan.Entry{Add: []string{"--pprof"}, By: "dev", Reason: "debug"}, nil
	})
	require.NoError(t, err)

	res = deploy(&local.DeployRequest{Container: "geth", Image: agenttest.GethImage, Command: gethCommand})
	assert.True(t, res.Changed)
	assert.Equal(t, local.DeployRollRequested, res.Roll, "the override's flags are not applied yet")
	assert.True(t, woken())

	res = deploy(&local.DeployRequest{
		Container: "geth", Image: agenttest.GethImage, Command: gethCommand, Reset: true,
	})
	assert.True(t, res.Changed)
	assert.Equal(t, local.DeployRollNone, res.Roll)
	assert.NotContains(t, e.readOverrides(), "--pprof\"]")

	res = deploy(&local.DeployRequest{Container: "dora", Image: "ethpandaops/dora:latest"})
	assert.Equal(t, local.DeployResult{
		Container: "dora", Exists: true, Changed: true, Roll: local.DeployRollNone, Ignore: []string{}, Pull: true,
	}, *res)

	res = deploy(&local.DeployRequest{Container: "erigon", Image: "erigontech/erigon:latest", Command: []string{}})
	assert.Equal(t, local.DeployResult{
		Container: "erigon", Changed: true, Roll: local.DeployRollNone, Ignore: []string{}, Pull: true,
	}, *res)
	assert.Contains(t, e.readOverrides(), "  \"erigon\":\n    image: \"erigontech/erigon:latest\"\n    command: []\n")

	res = deploy(&local.DeployRequest{Container: "erigon", Image: "erigontech/erigon:latest", Command: []string{}})
	assert.False(t, res.Changed)

	_, err = api.Deploy(t.Context(), "local:root", &local.DeployRequest{Container: "geth", Image: "Bad Image"})
	require.Error(t, err)

	assert.Equal(t, []string{
		"deploy accepted", "deploy accepted", "deploy accepted", "deploy accepted", "deploy accepted",
		"deploy accepted",
	}, auditActions(e.auditRecords()))
}
