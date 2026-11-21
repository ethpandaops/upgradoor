package local_test

import (
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
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// awaitLocal polls the local socket until pred holds.
func awaitLocal(
	t *testing.T, c *local.Client, what string, pred func(st *local.Status) bool,
) *local.Status {
	t.Helper()

	var last *local.Status

	require.Eventually(t, func() bool {
		st, err := c.Status(t.Context())
		if err != nil {
			return false
		}

		last = st

		return pred(st)
	}, agenttest.AwaitTimeout, 5*time.Millisecond, "local status never showed: %s", what)

	return last
}

func localContainer(t *testing.T, st *local.Status, name string) local.Container {
	t.Helper()

	i := slices.IndexFunc(st.Containers, func(c local.Container) bool { return c.Name == name })
	require.GreaterOrEqual(t, i, 0, "no container %s in the local status", name)

	return st.Containers[i]
}

func me(t *testing.T) string {
	t.Helper()

	u, err := user.Current()
	require.NoError(t, err)

	return u.Username
}

// The socket lists the managed containers and writes an override into the
// file for the Unix user who asked, with a comment line; the override is then
// served like one written by hand.
func TestLocalOverride(t *testing.T) {
	t.Parallel()

	e := agenttest.NewOverrideEnv(t)
	e.Ctrl.AutoRelease = true
	e.Start()

	c := local.NewClient(e.Cfg.Socket.Path)

	st := awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })
	assert.Equal(t, agenttest.HostName, st.Host)
	assert.Equal(t, e.Cfg.OverridesFile, st.OverridesFile)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, localContainer(t, st, "geth").Command)

	info, err := os.Stat(e.Cfg.Socket.Path)
	require.NoError(t, err)
	assert.Zero(t, info.Mode().Perm()&0o007, "the socket is not for everybody")

	_, err = c.InitOverrides(t.Context())
	require.NoError(t, err)

	until := e.Now().Add(time.Hour).UTC().Truncate(time.Second)

	answer, err := c.Override(t.Context(), &local.OverrideRequest{
		Container: "geth", Image: agenttest.FixImage, AddFlags: []string{"--pprof"}, Until: &until, Reason: "sync",
	})
	require.NoError(t, err)
	assert.Contains(t, answer.Message, "override of geth set (image "+agenttest.FixImage+", flags +--pprof)")

	raw, err := os.ReadFile(e.Cfg.OverridesFile)
	require.NoError(t, err)

	content := string(raw)
	assert.True(t, strings.HasPrefix(content, "# >>> upgradoor generated block"))
	assert.Contains(t, content, "#   upgradoor host override set <container>")
	assert.Contains(t, content, "deployed:\n  \"geth\":\n    image: \""+agenttest.GethImage+"\"\n"+
		"    command: [\"--http\", \"--maxpeers=50\"]\n")
	assert.Contains(t, content, "local:"+me(t)+": set image "+agenttest.FixImage+", flags +--pprof; was nothing\n")
	assert.Contains(t, content, "    by: \""+me(t)+"\"\n    reason: \"sync\"\n    until: "+until.Format(time.RFC3339)+"\n")

	need := agenttest.AwaitOverrideNeed(t, e, "geth", false)
	e.Ctrl.Grant(e.Lease("lease-local", need))
	require.Equal(t, protocol.PhaseDone, e.Ctrl.AwaitFinished("lease-local").Phase)

	st = awaitLocal(t, c, "the override applied", func(st *local.Status) bool {
		o := localContainer(t, st, "geth").Override

		return o != nil && o.Applied
	})
	geth := localContainer(t, st, "geth")
	assert.Equal(t, []string{"--http", "--maxpeers=50", "--pprof"}, geth.Command)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, geth.BaseCommand, "what the override changed is told apart")

	// What the host's own rules refuse says so and changes nothing.
	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "dora", Image: agenttest.FixImage})
	require.ErrorContains(t, err, "container dora is not managed")

	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Image: "ethpandaops/geth@sha256:abc"})
	require.Error(t, err)

	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Clear: true, Reason: "done"})
	require.NoError(t, err)
	agenttest.AwaitOverrideNeed(t, e, "geth", true)

	actions := agenttest.AuditActions(e.AuditRecords())
	assert.Equal(t, []string{
		"overrides.init accepted", "override.set accepted", "override.apply done",
		"override.set refused", "override.set refused", "override.clear accepted",
	}, actions)

	set := e.AuditRecords()[1]
	assert.Equal(t, "local:"+me(t), set.User)
	assert.Equal(t, "local", set.Source)
	assert.Equal(t, "nothing", set.Before)
	assert.Equal(t, "image "+agenttest.FixImage+", flags +--pprof", set.After)
}

// The local hold is an entry of the overrides file: set and released through the socket.
func TestLocalHoldThroughTheSocket(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Start()

	c := local.NewClient(e.Cfg.Socket.Path)
	agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	_, err := c.Hold(t.Context(), &local.HoldRequest{Reason: "debugging"})
	require.NoError(t, err)

	req := e.Ctrl.Await("the hold", func(req *protocol.SyncRequest) bool { return req.Hold.Local })
	assert.Equal(t, "debugging", req.Hold.LocalReason)
	assert.Contains(t, e.ReadOverrides(), "  local:\n    by: \""+me(t)+"\"\n", "the hold says who set it")
	assert.Empty(t, req.Needs, "a held host reports no needs")

	_, err = c.Roll(t.Context(), "geth")
	require.ErrorContains(t, err, "debugging")

	_, err = c.Hold(t.Context(), &local.HoldRequest{Release: true})
	require.NoError(t, err)
	agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	assert.Equal(t, []string{"hold.set accepted", "roll.request refused", "hold.release accepted"},
		agenttest.AuditActions(e.AuditRecords()))
}

// A roll requested on the host is a need served under a lease; whoever asked
// can follow it from the queue to the controller's release.
func TestLocalRoll(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Ctrl.Set(func(c *agenttest.Controller) {
		c.AutoRelease = true
		c.Queue = &protocol.Queue{Position: 2, Reason: "budget 10% of 10%"}
	})
	e.Start()

	c := local.NewClient(e.Cfg.Socket.Path)
	awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })

	_, err := c.Roll(t.Context(), "dora")
	require.ErrorContains(t, err, "not managed")

	answer, err := c.Roll(t.Context(), "geth")
	require.NoError(t, err)

	id := answer.Roll
	rollOf := func(st *local.Status) *local.Roll {
		for i := range st.Rolls {
			if st.Rolls[i].ID == id {
				return &st.Rolls[i]
			}
		}

		return nil
	}

	_, err = c.Roll(t.Context(), "geth")
	require.ErrorContains(t, err, "already under way")

	st := awaitLocal(t, c, "the roll queued", func(st *local.Status) bool {
		r := rollOf(st)

		return r != nil && r.State == local.RollQueued && r.Position == 2
	})
	assert.Equal(t, "budget 10% of 10%", rollOf(st).Detail)
	assert.Equal(t, "local:"+me(t), rollOf(st).RequestedBy)

	needs := agenttest.AwaitNeeds(t, e, protocol.LocalNeedID(id))
	assert.True(t, needs[0].Local)
	assert.Empty(t, needs[0].DirectiveID)
	assert.Equal(t, agenttest.GethV1, needs[0].Image.ToDigest, "a roll onto the image the tag points at, the same one here")

	e.Ctrl.Grant(e.Lease("lease-roll", needs...))

	op := e.Ctrl.AwaitFinished("lease-roll")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Contains(t, agenttest.Disruptive(e.Host.Calls()), "recreate geth "+agenttest.GethV1+" start=true")

	st = awaitLocal(t, c, "the roll done", func(st *local.Status) bool {
		r := rollOf(st)

		return r != nil && r.Final()
	})
	assert.Equal(t, local.RollDone, rollOf(st).State)
	assert.Equal(t, "lease-roll", rollOf(st).Lease)

	req := e.Ctrl.Await("the need is closed", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Empty(t, req.Needs)

	actions := agenttest.AuditActions(e.AuditRecords())
	assert.Equal(t, []string{
		"roll.request refused", "roll.request accepted", "roll.request refused", "roll.result done",
	}, actions)
}
