package agent

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
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
	set := "image " + agenttest.FixImage + ", command --http --maxpeers=50 --pprof"
	assert.Contains(t, answer.Message, set, "the flag is applied to the deployed command")

	content := e.readOverrides()
	assert.True(t, strings.HasPrefix(content, "# upgradoor overrides: "))
	assert.Contains(t, content, "#   upgradoor list | status | override set/clear <container>")
	assert.Contains(t, content, "local:"+me(t)+": set "+set+"; was nothing\n"+
		"  geth:\n    # default image: "+agenttest.GethImage+"\n"+
		"    # default command:\n    #   - --http\n    #   - --maxpeers=50\n"+
		"    image: "+agenttest.FixImage+"\n"+
		"    command:\n      - --http\n      - --maxpeers=50\n      - --pprof\n"+
		"    by: "+me(t)+"\n    reason: sync\n    until: "+until.Format(time.RFC3339)+"\n")
	assert.Contains(t, content, "  teku:\n    # default image: "+agenttest.TekuImage+"\n"+
		"    # default command: []\n    # image:\n", "an entry for a container without an override")
	assert.NotContains(t, content, "dora:", "no entry for a container without overrides")

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
	assert.Equal(t, set, records[1].After)
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
	assert.Equal(t, me(t), field(t, e.stateOnDisk(), "holds", "host", "by"), "the hold says who set it")
	assert.True(t, hasNeed(&req, protocol.ImageNeedID("geth", agenttest.GethV2)), "a held host still reports its needs")

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

	e.ctrl.Direct(protocol.Directive{ID: "d-hold", Kind: protocol.DirectiveHold, Holder: protocol.HolderStaff, Reason: "maintenance"})
	e.ctrl.Await("the controller hold", func(req *protocol.SyncRequest) bool { return req.Hold.Of(protocol.HolderStaff) != nil })

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
	held := status().Hold
	assert.True(t, held.Local)
	assert.Equal(t, []string{protocol.HolderHost, protocol.HolderStaff}, held.Holders())
	assert.Equal(t, "debugging", held.Of(protocol.HolderHost).Reason)
	assert.Equal(t, "maintenance", held.Of(protocol.HolderStaff).Reason)

	until := e.now().Add(time.Hour).UTC().Truncate(time.Second)
	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Image: agenttest.FixImage, Until: &until})
	require.NoError(t, err)

	o := localContainer(t, status(), "geth").Override
	require.NotNil(t, o)
	assert.Equal(t, agenttest.FixImage, o.Image)

	_, err = c.Hold(t.Context(), &local.HoldRequest{Release: true})
	require.NoError(t, err)
	assert.Equal(t, []string{protocol.HolderStaff}, status().Hold.Holders())
	assert.False(t, status().Hold.Local)

	_, err = c.Hold(t.Context(), &local.HoldRequest{Release: true})
	require.ErrorContains(t, err, "there is no local hold; the host is held by staff by ")
	assert.Contains(t, err.Error(), "(maintenance), lifted on the controller")

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
		c.Queue = &protocol.Queue{Position: 2, Reason: "validators down: 10% now, 15% with this host, limit 10%"}
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
	assert.Equal(t, "validators down: 10% now, 15% with this host, limit 10%", rollOf(st, id).Detail)
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

// A deploy reads managed.yaml again, lays out the overrides file and tells
// Ansible what to leave to the agent; it wakes the loop only for a container
// off its target.
func TestLocalDeploy(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	a, ok := e.newAgent().(*agent)
	require.True(t, ok)
	require.NoError(t, a.open())

	api := localAPI{a}
	deploy := func(container string, d plan.Deployed, reset bool) *local.DeployResult {
		t.Helper()

		var res *local.DeployResult

		require.NoError(t, local.WriteManaged(e.cfg.Local.ManagedFile, container, d, func(changed bool) error {
			var err error

			res, err = api.Deploy(t.Context(), "local:root", &local.DeployRequest{Container: container, Reset: reset})
			if err == nil {
				res.Changed = res.Changed || changed
			}

			return err
		}))

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

	res := deploy("geth", plan.Deployed{Image: agenttest.GethImage, Command: gethCommand, HasCommand: true}, false)
	assert.Equal(t, local.DeployResult{
		Container: "geth", Exists: true, Managed: true, Changed: true, Roll: local.DeployRollNone, Ignore: managed,
	}, *res)
	assert.False(t, woken(), "in line with the target")
	assert.Contains(t, e.readOverrides(), "  geth:\n    # default image: "+agenttest.GethImage+"\n"+
		"    # default command:\n    #   - --http\n    #   - --maxpeers=50\n    # image:\n")

	res = deploy("geth", plan.Deployed{Image: agenttest.FixImage, Command: gethCommand, HasCommand: true}, false)
	assert.Equal(t, local.DeployResult{
		Container: "geth", Exists: true, Managed: true, Changed: true, Roll: local.DeployRollRequested,
		Ignore: managed,
	}, *res)
	assert.True(t, woken())
	assert.Contains(t, e.readOverrides(), "    # default image: "+agenttest.FixImage+"\n", "the default lines follow")

	// An override is kept by a deploy and cleared by a reset.
	_, _, err := a.files.SetEntry("geth", "dev", func(*plan.Entry) (*plan.Entry, error) {
		return &plan.Entry{
			Command: []string{"--http", "--maxpeers=50", "--pprof"}, HasCommand: true, By: "dev", Reason: "debug",
		}, nil
	})
	require.NoError(t, err)

	res = deploy("geth", plan.Deployed{Image: agenttest.GethImage, Command: gethCommand, HasCommand: true}, false)
	assert.True(t, res.Changed)
	assert.Equal(t, local.DeployRollRequested, res.Roll, "the override's flags are not applied yet")
	assert.True(t, woken())
	assert.Contains(t, e.readOverrides(), "      - --pprof\n")

	res = deploy("geth", plan.Deployed{Image: agenttest.GethImage, Command: gethCommand, HasCommand: true}, true)
	assert.True(t, res.Changed, "the override was cleared")
	assert.Equal(t, local.DeployRollNone, res.Roll)
	assert.Empty(t, a.files.Load().Entries)
	assert.Contains(t, e.readOverrides(), "    #   - --maxpeers=50\n    # image:\n    # command:\n")

	res = deploy("geth", plan.Deployed{Image: agenttest.GethImage, Command: gethCommand, HasCommand: true}, true)
	assert.False(t, res.Changed, "nothing to change")

	res = deploy("dora", plan.Deployed{Image: "ethpandaops/dora:latest"}, false)
	assert.Equal(t, local.DeployResult{
		Container: "dora", Exists: true, Changed: true, Roll: local.DeployRollNone, Ignore: []string{}, Pull: true,
	}, *res)
	assert.NotContains(t, e.readOverrides(), "dora:")

	res = deploy("erigon", plan.Deployed{Image: "erigontech/erigon:latest", Command: []string{}, HasCommand: true}, false)
	assert.Equal(t, local.DeployResult{
		Container: "erigon", Changed: true, Roll: local.DeployRollNone, Ignore: []string{}, Pull: true,
	}, *res)

	raw, err := os.ReadFile(e.cfg.Local.ManagedFile)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "  erigon:\n    image: erigontech/erigon:latest\n    command: []\n")

	_, err = api.Deploy(t.Context(), "local:root", &local.DeployRequest{Container: "nethermind"})
	require.ErrorContains(t, err, "managed.yaml has no entry for container nethermind")

	assert.Equal(t, []string{
		"deploy accepted", "deploy accepted", "deploy accepted", "deploy accepted", "deploy accepted",
		"deploy accepted",
	}, auditActions(e.auditRecords()))
}

// Without an overrides file nothing can be overridden: the socket and the
// controller are refused, and the sync offers no override.
func TestOverridesDisabled(t *testing.T) {
	t.Parallel()

	e := overrideEnv(t)
	e.cfg.Local.OverridesFile = ""
	e.deploy(map[string]plan.Deployed{"geth": {Image: agenttest.GethImage}})
	e.start()

	c := local.NewClient(e.cfg.Local.Socket.Path)
	st := awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })
	assert.Empty(t, st.OverridesFile)

	req := e.ctrl.Await("a sync", func(req *protocol.SyncRequest) bool { return len(req.Containers) > 0 })
	for _, ct := range req.Containers {
		assert.False(t, ct.Overridable, ct.Name)
	}

	_, err := c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Image: agenttest.FixImage})
	require.EqualError(t, err, "overrides are disabled on this host")

	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Clear: true})
	require.EqualError(t, err, "overrides are disabled on this host")

	_, err = c.InitOverrides(t.Context())
	require.EqualError(t, err, "overrides are disabled on this host")

	until := e.now().Add(time.Hour)
	e.ctrl.Direct(protocol.Directive{
		ID: "d-o", Kind: protocol.DirectiveOverride, Container: "geth",
		Override: &protocol.OverrideInstruction{Part: protocol.OverrideImage, Image: agenttest.FixImage, Until: &until},
	})
	req = e.ctrl.Await("the answer", func(req *protocol.SyncRequest) bool { return agenttest.AckOf(req, "d-o") != nil })
	assert.Equal(t, protocol.DirectiveRejected, agenttest.AckOf(&req, "d-o").Status)
	assert.Equal(t, "overrides are disabled on this host", agenttest.AckOf(&req, "d-o").Error)

	home := filepath.Join(filepath.Dir(filepath.Dir(e.cfg.Local.ManagedFile)), "home")
	assert.NoFileExists(t, filepath.Join(home, "overrides.yaml"), "no overrides file is written")
}
