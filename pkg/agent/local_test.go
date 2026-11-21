package agent

import (
	"os"
	"os/user"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	}, awaitTimeout, 5*time.Millisecond, "local status never showed: %s", what)

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
	e := overrideEnv(t)
	e.ctrl.autoRelease = true
	e.start()

	c := local.NewClient(e.cfg.Socket.Path)

	st := awaitLocal(t, c, "the containers", func(st *local.Status) bool { return len(st.Containers) == 3 })
	assert.Equal(t, testHost, st.Host)
	assert.Equal(t, e.cfg.OverridesFile, st.OverridesFile)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, localContainer(t, st, "geth").Command)

	info, err := os.Stat(e.cfg.Socket.Path)
	require.NoError(t, err)
	assert.Zero(t, info.Mode().Perm()&0o007, "the socket is not for everybody")

	_, err = c.InitOverrides(t.Context())
	require.NoError(t, err)

	until := e.now().Add(time.Hour).UTC().Truncate(time.Second)

	answer, err := c.Override(t.Context(), &local.OverrideRequest{
		Container: "geth", Image: fixImage, AddFlags: []string{"--pprof"}, Until: &until, Reason: "sync",
	})
	require.NoError(t, err)
	assert.Contains(t, answer.Message, "override of geth set (image "+fixImage+", flags +--pprof)")

	raw, err := os.ReadFile(e.cfg.OverridesFile)
	require.NoError(t, err)

	content := string(raw)
	assert.True(t, strings.HasPrefix(content, "# >>> upgradoor generated block"))
	assert.Contains(t, content, "#   upgradoor host override set <container>")
	assert.Contains(t, content, "#   geth:\n#     image: docker.io/"+gethImage+"\n#     command:\n#       - \"--http\"\n")
	assert.Contains(t, content, "local:"+me(t)+": set image "+fixImage+", flags +--pprof; was nothing\n")
	assert.Contains(t, content, "    by: \""+me(t)+"\"\n    reason: \"sync\"\n    until: "+until.Format(time.RFC3339)+"\n")

	need := awaitOverrideNeed(t, e, "geth", false)
	e.ctrl.grant(e.lease("lease-local", need))
	require.Equal(t, protocol.PhaseDone, e.ctrl.awaitFinished("lease-local").Phase)

	st = awaitLocal(t, c, "the override applied", func(st *local.Status) bool {
		o := localContainer(t, st, "geth").Override

		return o != nil && o.Applied
	})
	geth := localContainer(t, st, "geth")
	assert.Equal(t, []string{"--http", "--maxpeers=50", "--pprof"}, geth.Command)
	assert.Equal(t, []string{"--http", "--maxpeers=50"}, geth.BaseCommand, "what the override changed is told apart")

	// What the host's own rules refuse says so and changes nothing.
	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "dora", Image: fixImage})
	require.ErrorContains(t, err, "container dora is not managed")

	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Image: "ethpandaops/geth@sha256:abc"})
	require.Error(t, err)

	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Clear: true, Reason: "done"})
	require.NoError(t, err)
	awaitOverrideNeed(t, e, "geth", true)

	actions := auditActions(e.auditRecords())
	assert.Equal(t, []string{
		"overrides.init accepted", "override.set accepted", "override.apply done",
		"override.set refused", "override.set refused", "override.clear accepted",
	}, actions)

	set := e.auditRecords()[1]
	assert.Equal(t, "local:"+me(t), set.User)
	assert.Equal(t, "local", set.Source)
	assert.Equal(t, "nothing", set.Before)
	assert.Equal(t, "image "+fixImage+", flags +--pprof", set.After)
}

// The local hold is the hold file: set and released through the socket.
func TestLocalHoldThroughTheSocket(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	c := local.NewClient(e.cfg.Socket.Path)
	awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	_, err := c.Hold(t.Context(), &local.HoldRequest{Reason: "debugging"})
	require.NoError(t, err)

	req := e.ctrl.await("the hold", func(req *protocol.SyncRequest) bool { return req.Hold.Local })
	assert.Equal(t, "debugging ("+me(t)+")", req.Hold.LocalReason)
	assert.Empty(t, req.Needs, "a held host reports no needs")

	_, err = c.Roll(t.Context(), "geth")
	require.ErrorContains(t, err, "debugging")

	_, err = c.Hold(t.Context(), &local.HoldRequest{Release: true})
	require.NoError(t, err)
	awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	assert.Equal(t, []string{"hold.set accepted", "roll.request refused", "hold.release accepted"},
		auditActions(e.auditRecords()))
}

// A roll requested on the host is a need served under a lease; whoever asked
// can follow it from the queue to the controller's release.
func TestLocalRoll(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.ctrl.set(func(c *controller) {
		c.autoRelease = true
		c.queue = &protocol.Queue{Position: 2, Reason: "budget 10% of 10%"}
	})
	e.start()

	c := local.NewClient(e.cfg.Socket.Path)
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

	needs := awaitNeeds(t, e, protocol.LocalNeedID(id))
	assert.True(t, needs[0].Local)
	assert.Empty(t, needs[0].DirectiveID)
	assert.Equal(t, gethV1, needs[0].Image.ToDigest, "a roll onto the image the tag points at, the same one here")

	e.ctrl.grant(e.lease("lease-roll", needs...))

	op := e.ctrl.awaitFinished("lease-roll")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Contains(t, disruptive(e.host.Calls()), "recreate geth "+gethV1+" start=true")

	st = awaitLocal(t, c, "the roll done", func(st *local.Status) bool {
		r := rollOf(st)

		return r != nil && r.Final()
	})
	assert.Equal(t, local.RollDone, rollOf(st).State)
	assert.Equal(t, "lease-roll", rollOf(st).Lease)

	req := e.ctrl.await("the need is closed", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Empty(t, req.Needs)

	actions := auditActions(e.auditRecords())
	assert.Equal(t, []string{
		"roll.request refused", "roll.request accepted", "roll.request refused", "roll.result done",
	}, actions)
}
