package agent_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestNewValidatesItsInput(t *testing.T) {
	e := newEnv(t, baseScenario())

	_, err := agent.New(e.log, e.cfg, e.creds, agent.Deps{Runtime: e.host, Registry: e.host})
	require.Error(t, err, "a system is required")

	_, err = agent.New(e.log, nil, e.creds, agent.Deps{})
	require.Error(t, err)

	_, err = agent.New(e.log, e.cfg, nil, agent.Deps{Runtime: e.host, Registry: e.host, System: e.host})
	require.Error(t, err)

	raw := *e.cfg
	raw.Verify.Window = 0
	_, err = agent.New(e.log, &raw, e.creds, agent.Deps{Runtime: e.host, Registry: e.host, System: e.host})
	require.Error(t, err, "a configuration without defaults is refused")

	svc, err := agent.New(e.log, e.cfg, e.creds, agent.Deps{Runtime: e.host, Registry: e.host, System: e.host})
	require.NoError(t, err)
	require.NoError(t, svc.Stop(), "stopping an agent that never started is fine")
	require.NoError(t, svc.Start(t.Context()))
	require.Error(t, svc.Start(t.Context()), "an agent starts once")
	require.NoError(t, svc.Stop())
	require.NoError(t, svc.Stop())
}

func TestSyncReportsTheHost(t *testing.T) {
	sc := baseScenario()
	sc.Packages.Upgradable = dockerUpgrade()

	e := newEnv(t, sc)
	e.start()

	req := e.ctrl.await("registry checks", func(req *protocol.SyncRequest) bool {
		for _, c := range req.Containers {
			if c.Managed && c.Registry == nil {
				return false
			}
		}

		return len(req.Containers) == 4
	})

	assert.Equal(t, protocol.Version, req.ProtocolVersion)
	assert.Equal(t, "test", req.AgentVersion)
	assert.Equal(t, "boot", req.BootID)
	assert.Equal(t, e.cfg.Verbs, req.Verbs)
	assert.False(t, req.Hold.Active())
	assert.Equal(t, "debian", req.OS.ID)
	assert.Equal(t, protocol.Pending{Upgradable: 2, Security: 1, Blocked: 1}, req.Pending)
	assert.NotEmpty(t, req.InventoryHash)
	assert.Nil(t, req.Operation)
	assert.Nil(t, req.Node.EL, "no prober, no node health")

	geth := containerOf(t, &req, "geth")
	assert.True(t, geth.Managed)
	assert.Equal(t, protocol.KindEL, geth.Kind)
	assert.Equal(t, "docker.io/"+gethImage, geth.ConfiguredImage)
	assert.False(t, geth.Custom, "docker.io/x and x are the same reference")
	require.NotNil(t, geth.Registry)
	assert.Equal(t, gethV1, geth.Registry.Digest)
	assert.True(t, geth.Registry.Pulled)
	assert.Empty(t, geth.Registry.Error)

	dora := containerOf(t, &req, "dora")
	assert.False(t, dora.Managed)
	assert.Empty(t, dora.Kind)
	assert.Nil(t, dora.Registry)

	// The only need is the blocked package; openssl is not the agent's.
	require.Equal(t, []string{packagesNeed().ID}, needIDs(&req))
	assert.Equal(t, packagesNeed(), req.Needs[0])
}

func TestImageNeedAppearsOnlyAfterThePull(t *testing.T) {
	e := newEnv(t, baseScenario())

	// The registry moves the tag, but the image cannot be pulled yet.
	e.host.Fail("pull:"+gethImage, errors.New("registry is slow today"))
	e.host.SetRegistry(gethImage, gethV2, "rev-2")
	e.start()

	req := e.ctrl.await("failed pull reported", func(req *protocol.SyncRequest) bool {
		c := containerOf(t, req, "geth")

		return c.Registry != nil && c.Registry.Digest == gethV2 && c.Registry.Error != ""
	})

	geth := containerOf(t, &req, "geth")
	assert.False(t, geth.Registry.Pulled)
	assert.Contains(t, geth.Registry.Error, "registry is slow today")
	assert.Empty(t, req.Needs, "no slot is requested for an image that is not local")
	assert.Equal(t, gethV1, geth.Digest, "pulling touches no container")

	e.host.Fail("pull:"+gethImage, nil)

	needID := protocol.ImageNeedID("geth", gethV2)
	req = e.ctrl.await("image need", func(req *protocol.SyncRequest) bool { return hasNeed(req, needID) })

	need := findNeed(t, &req, needID)
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Image: gethImage, FromDigest: gethV1, ToDigest: gethV2, ToRevision: "rev-2",
	}, need.Image)
	assert.Empty(t, need.DirectiveID)
	assert.Contains(t, e.host.Calls(), "pull "+gethImage)
	assert.Empty(t, disruptive(e.host.Calls()), "nothing but pulls happens without a lease")

	// In no sync ever was a need reported before its image was local.
	for _, s := range e.ctrl.allSyncs() {
		if hasNeed(&s, needID) {
			c := containerOf(t, &s, "geth")
			require.NotNil(t, c.Registry)
			require.True(t, c.Registry.Pulled)
		}
	}
}

func TestRegistryErrorKeepsTheLastGoodDigest(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.start()

	e.ctrl.await("first check", func(req *protocol.SyncRequest) bool {
		return containerOf(t, req, "teku").Registry != nil
	})

	e.host.Fail("resolve:"+tekuImage, errors.New("503 from the registry"))

	req := e.ctrl.await("registry error", func(req *protocol.SyncRequest) bool {
		return containerOf(t, req, "teku").Registry.Error != ""
	})

	teku := containerOf(t, &req, "teku")
	assert.Contains(t, teku.Registry.Error, "503 from the registry")
	assert.Equal(t, tekuV1, teku.Registry.Digest, "the last good digest stays")
	assert.Empty(t, req.Needs)
}

func TestVerbsBoundTheNeeds(t *testing.T) {
	sc := baseScenario()
	sc.Packages.Upgradable = dockerUpgrade()
	sc.RebootRequired = true
	sc.NewestKernel = "6.12.48-amd64"

	e := newEnv(t, sc)
	e.host.SetRegistry(gethImage, gethV2, "")
	e.cfg.Verbs = []protocol.Verb{protocol.VerbReboot}
	e.start()

	// The new digest is seen and reported ...
	e.ctrl.await("new digest reported", func(req *protocol.SyncRequest) bool {
		c := containerOf(t, req, "geth")

		return c.Registry != nil && c.Registry.Digest == gethV2
	})

	// ... but a host that never rolls does not pull it: the image would
	// only be picked up by whoever recreates the container next.
	req := e.ctrl.awaitSyncs(8)

	geth := containerOf(t, &req, "geth")
	require.NotNil(t, geth.Registry)
	assert.Equal(t, gethV2, geth.Registry.Digest)
	assert.False(t, geth.Registry.Pulled)
	assert.Empty(t, geth.Registry.Error)
	assert.Empty(t, e.host.Calls(), "nothing is pulled without the roll verb")

	require.Equal(t, []string{protocol.RebootNeedID}, needIDs(&req))
	assert.Equal(t, []string{protocol.RebootReasonFlag, protocol.RebootReasonKernel},
		req.Needs[0].Reboot.Reasons)
}

func TestCustomContainerIsNeverRolled(t *testing.T) {
	e := newEnv(t, baseScenario())

	// Somebody swapped geth for a debug build. Both the configured tag and
	// the custom one have news in the registry.
	require.NoError(t, e.host.SetContainerImage("geth", "myfork/geth:debug", "sha256:debug-1"))
	e.host.SetRegistry("myfork/geth:debug", "sha256:debug-2", "")
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	e.ctrl.awaitSyncs(3)
	req := e.ctrl.awaitSyncs(8)

	geth := containerOf(t, &req, "geth")
	assert.True(t, geth.Managed)
	assert.True(t, geth.Custom)
	assert.Nil(t, geth.Registry, "a custom image is not followed")
	assert.Empty(t, req.Needs)
	assert.Empty(t, e.host.Calls(), "not even pulled")

	// A manual roll is rejected ...
	e.ctrl.direct(protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"})

	req = e.ctrl.await("roll rejected", func(req *protocol.SyncRequest) bool { return ackOf(req, "d-roll") != nil })
	ack := ackOf(&req, "d-roll")
	assert.Equal(t, protocol.DirectiveRejected, ack.Status)
	assert.Contains(t, ack.Error, "custom image")
	e.ctrl.direct()

	// ... and a lease that grants a roll anyway changes nothing.
	need := imageNeedFor("geth", gethImage, gethV1, gethV2)
	e.ctrl.grant(e.lease("lease-custom", need))

	op := e.ctrl.awaitFinished("lease-custom")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)

	step := stepOf(t, op, need.ID)
	assert.Equal(t, protocol.StepSuperseded, step.Status)
	assert.Contains(t, step.Error, "custom image")
	assert.Empty(t, disruptive(e.host.Calls()))
	assert.Equal(t, "sha256:debug-1", e.host.Container("geth").Digest)
}

func TestLocalHold(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.host.SetRegistry(gethImage, gethV2, "")
	require.NoError(t, os.WriteFile(e.cfg.HoldFile, []byte("debugging geth, ask pk\nsecond line\n"), 0o600))
	e.start()

	// Held: the image is pulled (harmless) but nothing is asked for.
	req := e.ctrl.await("image pulled under hold", func(req *protocol.SyncRequest) bool {
		c := containerOf(t, req, "geth")

		return c.Registry != nil && c.Registry.Pulled && c.Registry.Digest == gethV2
	})
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging geth, ask pk"}, req.Hold)
	assert.Empty(t, req.Needs)

	// The controller cannot lift a local hold, and cannot work around it.
	e.ctrl.direct(
		protocol.Directive{ID: "d-release", Kind: protocol.DirectiveReleaseHold},
		protocol.Directive{ID: "d-reboot", Kind: protocol.DirectiveReboot},
		protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"},
	)

	req = e.ctrl.await("directives answered", func(req *protocol.SyncRequest) bool {
		return ackOf(req, "d-release") != nil && ackOf(req, "d-reboot") != nil && ackOf(req, "d-roll") != nil
	})
	assert.True(t, req.Hold.Local, "release-hold does not lift the local hold")
	assert.FileExists(t, e.cfg.HoldFile)
	assert.Equal(t, protocol.DirectiveAccepted, ackOf(&req, "d-release").Status)
	assert.Equal(t, protocol.DirectiveRejected, ackOf(&req, "d-reboot").Status)
	assert.Contains(t, ackOf(&req, "d-reboot").Error, "local hold: debugging geth, ask pk")
	assert.Equal(t, protocol.DirectiveRejected, ackOf(&req, "d-roll").Status)
	assert.Empty(t, req.Needs)
	e.ctrl.direct()

	// A lease is refused without touching anything.
	need := imageNeedFor("geth", gethImage, gethV1, gethV2)
	e.ctrl.grant(e.lease("lease-held", need, rebootNeed()))

	op := e.ctrl.awaitFinished("lease-held")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepRefused, s.Status)
		assert.Contains(t, s.Error, "local hold")
	}

	assert.Empty(t, disruptive(e.host.Calls()))

	// Lifted on the host, the needs come back by themselves.
	require.NoError(t, os.Remove(e.cfg.HoldFile))

	req = e.ctrl.await("needs after the hold", func(req *protocol.SyncRequest) bool { return len(req.Needs) == 2 })
	assert.False(t, req.Hold.Active())
	assert.Equal(t, []string{need.ID, protocol.RebootNeedID}, needIDs(&req))
}

func TestControllerHold(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	stop := e.start()

	e.ctrl.await("reboot need", func(req *protocol.SyncRequest) bool { return hasNeed(req, protocol.RebootNeedID) })

	e.ctrl.direct(protocol.Directive{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "maintenance"})

	req := e.ctrl.await("held", func(req *protocol.SyncRequest) bool { return req.Hold.Controller })
	assert.False(t, req.Hold.Local)
	assert.Empty(t, req.Needs)

	// The hold outlives the directive and the agent process.
	e.ctrl.direct()
	stop()
	e.start()

	req = e.ctrl.awaitSyncs(3)
	assert.True(t, req.Hold.Controller, "the controller hold is persisted")
	assert.Empty(t, req.Needs)

	e.ctrl.grant(e.lease("lease-held", rebootNeed()))

	op := e.ctrl.awaitFinished("lease-held")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, op.Steps[0].Status)
	assert.Contains(t, op.Steps[0].Error, "controller hold")
	assert.Empty(t, e.host.Calls())

	e.ctrl.direct(protocol.Directive{ID: "d-release", Kind: protocol.DirectiveReleaseHold})

	req = e.ctrl.await("released", func(req *protocol.SyncRequest) bool {
		return !req.Hold.Active() && hasNeed(req, protocol.RebootNeedID)
	})
	assert.Equal(t, "boot", req.BootID, "releasing a hold does not reboot by itself")
}

func TestControllerDowntimeIsTolerated(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.ctrl.set(func(c *controller) { c.down = true })
	e.start()

	// Let the agent run into the wall a few times.
	time.Sleep(60 * time.Millisecond)
	require.Zero(t, e.ctrl.syncCount())

	e.ctrl.set(func(c *controller) { c.down = false })

	req := e.ctrl.await("sync after the outage", func(req *protocol.SyncRequest) bool {
		return hasNeed(req, protocol.RebootNeedID)
	})
	assert.Equal(t, "boot", req.BootID)
}

func TestInventoryIsSentWhenAsked(t *testing.T) {
	sc := baseScenario()
	sc.Packages.Upgradable = dockerUpgrade()

	e := newEnv(t, sc)
	e.start()

	req := e.ctrl.awaitSyncs(2)

	e.ctrl.mu.Lock()
	assert.Empty(t, e.ctrl.inventories, "no inventory unless asked")
	e.ctrl.wantInventory = true
	e.ctrl.mu.Unlock()

	require.Eventually(t, func() bool {
		e.ctrl.mu.Lock()
		defer e.ctrl.mu.Unlock()

		return len(e.ctrl.inventories) > 0
	}, awaitTimeout, 2*time.Millisecond)

	e.ctrl.mu.Lock()
	inv := e.ctrl.inventories[0]
	e.ctrl.wantInventory = false
	e.ctrl.mu.Unlock()

	hash, err := inv.ComputeHash()
	require.NoError(t, err)
	assert.Equal(t, hash, inv.Hash)
	assert.Equal(t, req.InventoryHash, inv.Hash, "the sync announces the hash of this inventory")
	assert.Len(t, inv.Packages, 3)
	assert.Len(t, inv.Upgradable, 2)
}

func TestCorruptStateFileStartsClean(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	require.NoError(t, os.MkdirAll(e.cfg.StateDir, 0o700))
	require.NoError(t, os.WriteFile(e.statePath(), []byte(`{"version":1,"current":{"lease":`), 0o600))
	e.start()

	req := e.ctrl.await("needs", func(req *protocol.SyncRequest) bool { return hasNeed(req, protocol.RebootNeedID) })
	assert.Nil(t, req.Operation)

	kept, err := os.ReadFile(e.statePath() + ".corrupt")
	require.NoError(t, err)
	assert.Equal(t, `{"version":1,"current":{"lease":`, string(kept),
		"the damaged file is kept for inspection")
}
