package agent_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestSyncReportsTheHost(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()

	e := agenttest.NewEnv(t, sc)
	e.Cfg.Groups[agenttest.GroupExecution] = config.Group{Owners: []string{"panda", "geth"}}
	e.Cfg.Groups[agenttest.GroupConsensus] = config.Group{Owners: []string{"teku"}}
	require.NoError(t, e.Cfg.Validate())
	// Spelled differently from the container's reference on purpose.
	e.Deploy(overrides.GeneratedContainer{Name: "geth", Image: "docker.io/" + agenttest.GethImage})
	e.Start()

	req := e.Ctrl.Await("registry checks", func(req *protocol.SyncRequest) bool {
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
	assert.Equal(t, e.Cfg.Verbs, req.Verbs)
	assert.False(t, req.Hold.Active())
	assert.Equal(t, "debian", req.OS.ID)
	assert.Equal(t, protocol.Pending{Upgradable: 2, Security: 1, Blocked: 1}, req.Pending)
	assert.NotEmpty(t, req.InventoryHash)
	assert.Nil(t, req.Operation)
	assert.Nil(t, req.Node.EL, "no prober, no node health")

	geth := agenttest.ContainerOf(t, &req, "geth")
	assert.True(t, geth.Managed)
	assert.Equal(t, protocol.KindEL, geth.Kind)
	assert.Equal(t, "docker.io/"+agenttest.GethImage, geth.ConfiguredImage)
	for _, n := range req.Needs {
		assert.NotEqual(t, protocol.NeedSpec, n.Kind, "docker.io/x and x are the same reference")
	}
	require.NotNil(t, geth.Registry)
	assert.Equal(t, agenttest.GethV1, geth.Registry.Digest)
	assert.True(t, geth.Registry.Pulled)
	assert.Empty(t, geth.Registry.Error)

	dora := agenttest.ContainerOf(t, &req, "dora")
	assert.False(t, dora.Managed)
	assert.Empty(t, dora.Kind)
	assert.Nil(t, dora.Registry)

	// The owners of a group go out with each of its managed containers,
	// sorted.
	assert.Equal(t, []string{"geth", "panda"}, geth.Owners)
	assert.Equal(t, agenttest.GroupExecution, geth.Group)
	assert.Equal(t, []string{"teku"}, agenttest.ContainerOf(t, &req, "teku").Owners)
	assert.Equal(t, []string{"teku"}, agenttest.ContainerOf(t, &req, "validator").Owners, "the same group")
	assert.Equal(t, protocol.KindVC, agenttest.ContainerOf(t, &req, "validator").Kind, "from its label")
	assert.Nil(t, dora.Owners, "not managed")

	// The only need is the blocked package; openssl is not the agent's.
	require.Equal(t, []string{agenttest.PackagesNeed().ID}, agenttest.NeedIDs(&req))
	assert.Equal(t, agenttest.PackagesNeed(), req.Needs[0])
}

// A need whose image is not local says so; the image is pulled only in a
// pull window, and only the image the window names.
func TestImageNeedWaitsForItsPull(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Ctrl.Set(func(c *agenttest.Controller) { c.NoPulls = true })

	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "rev-2")
	e.Start()

	needID := protocol.ImageNeedID("geth", agenttest.GethV2)
	req := e.Ctrl.Await("the need, waiting for its image", func(req *protocol.SyncRequest) bool {
		return agenttest.HasNeed(req, needID)
	})

	need := agenttest.FindNeed(t, &req, needID)
	assert.Equal(t, []protocol.PullImage{{Ref: agenttest.GethImage, Digest: agenttest.GethV2}}, need.Unpulled)
	assert.False(t, agenttest.ContainerOf(t, &req, "geth").Registry.Pulled)

	e.Ctrl.AwaitSyncs(3)
	assert.NotContains(t, e.Host.Calls(), "pull "+agenttest.GethImage, "no pull without a window")

	// A window, but the pull fails at first.
	e.Host.Fail("pull:"+agenttest.GethImage, errors.New("registry is slow today"))
	e.Ctrl.Set(func(c *agenttest.Controller) { c.NoPulls = false })

	req = e.Ctrl.Await("failed pull reported", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Registry != nil && c.Registry.Error != ""
	})
	assert.Contains(t, agenttest.ContainerOf(t, &req, "geth").Registry.Error, "registry is slow today")
	assert.Equal(t, agenttest.GethV1, agenttest.ContainerOf(t, &req, "geth").Digest, "pulling touches no container")

	e.Host.Fail("pull:"+agenttest.GethImage, nil)

	req = e.Ctrl.Await("pulled", func(req *protocol.SyncRequest) bool { return agenttest.HasPulledNeed(req, needID) })

	need = agenttest.FindNeed(t, &req, needID)
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Group: agenttest.GroupExecution, Image: agenttest.GethImage, FromDigest: agenttest.GethV1, ToDigest: agenttest.GethV2, ToRevision: "rev-2",
	}, need.Image)
	assert.Empty(t, need.DirectiveID)
	assert.Contains(t, e.Host.Calls(), "pull "+agenttest.GethImage)
	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()), "nothing but pulls happens without a lease")
}

// The controller hands out the lookups: without the duty the agent does not
// look a tag up, and a digest another host saw is taken from the answer.
func TestLookupsAreCoordinated(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Ctrl.Set(func(c *agenttest.Controller) { c.NoLookups = true })
	e.Start()

	e.Ctrl.AwaitSyncs(3)

	lookups := e.Host.Resolves

	// Until the first answer the agent looked its tags up itself.
	before := lookups()

	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "rev-2")
	e.Ctrl.AwaitSyncs(3)
	assert.Equal(t, before, lookups(), "no lookup without the duty while the controller answers")

	// Another host saw the new digest: the controller relays it.
	e.Ctrl.Set(func(c *agenttest.Controller) {
		c.Relay = []protocol.TagDigest{{Ref: agenttest.GethImage, Digest: agenttest.GethV2, CheckedAt: e.Now()}}
	})

	needID := protocol.ImageNeedID("geth", agenttest.GethV2)
	agenttest.AwaitNeeds(t, e, needID)
	assert.Equal(t, before, lookups(), "the relayed digest needed no lookup")
}

func TestVerbsBoundTheNeeds(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()
	sc.RebootRequired = true
	sc.NewestKernel = "6.12.48-amd64"

	e := agenttest.NewEnv(t, sc)
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Cfg.Verbs = []protocol.Verb{protocol.VerbReboot}
	e.Start()

	// The new digest is seen and reported ...
	e.Ctrl.Await("new digest reported", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Registry != nil && c.Registry.Digest == agenttest.GethV2
	})

	// ... but a host that never rolls does not pull it: the image would
	// only be picked up by whoever recreates the container next.
	req := e.Ctrl.AwaitSyncs(8)

	geth := agenttest.ContainerOf(t, &req, "geth")
	require.NotNil(t, geth.Registry)
	assert.Equal(t, agenttest.GethV2, geth.Registry.Digest)
	assert.False(t, geth.Registry.Pulled)
	assert.Empty(t, geth.Registry.Error)
	assert.Empty(t, e.Host.Calls(), "nothing is pulled without the roll verb")

	require.Equal(t, []string{protocol.RebootNeedID}, agenttest.NeedIDs(&req))
	assert.Equal(t, []string{protocol.RebootReasonFlag, protocol.RebootReasonKernel},
		req.Needs[0].Reboot.Reasons)
}

// A container that does not run what agent.yaml says (Ansible named a new
// image and left the restart to the agent, or somebody swapped it by hand)
// is reported with a spec need and recreated under a lease, its
// configuration kept, on the configured image at the digest its tag has.
func TestContainerIsRolledOntoItsSpec(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())

	require.NoError(t, e.Host.SetContainerImage("geth", "myfork/geth:debug", "sha256:debug-1"))
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	// A soft deploy: Ansible wrote the block and left the container alone.
	e.Deploy(overrides.GeneratedContainer{Name: "geth", Image: "docker.io/" + agenttest.GethImage})
	e.Start()

	var need protocol.Need

	e.Ctrl.Await("a spec need", func(req *protocol.SyncRequest) bool {
		for _, n := range req.Needs {
			if n.Kind == protocol.NeedSpec {
				need = n

				return true
			}
		}

		return false
	})
	require.NotNil(t, need.Spec)
	assert.Equal(t, "geth", need.Spec.Container)
	assert.Equal(t, agenttest.GethV2, need.Spec.ToDigest)
	assert.Equal(t, "myfork/geth:debug", need.Spec.FromImage)
	assert.False(t, need.Spec.Flags)
	assert.False(t, need.Local, "hosts share the rollout of a spec")

	e.Ctrl.Grant(e.Lease("lease-spec", need))

	op := e.Ctrl.AwaitFinished("lease-spec")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, need.ID).Status)
	assert.Equal(t, agenttest.GethV2, e.Host.Container("geth").Digest)
	assert.Equal(t, "docker.io/"+agenttest.GethImage, e.Host.Container("geth").Image)

	req := e.Ctrl.Await("in line with the deployed block", func(req *protocol.SyncRequest) bool {
		return !agenttest.HasNeed(req, need.ID)
	})
	assert.Empty(t, req.Needs)
}

func TestLocalHold(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := agenttest.NewEnv(t, sc)
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.WriteOverrides("hold:\n  local:\n    by: pk\n    reason: \"debugging geth, ask pk\"\n")
	e.Start()

	// Held: the new digest is seen, but nothing is asked for and nothing pulled.
	req := e.Ctrl.Await("new digest seen under hold", func(req *protocol.SyncRequest) bool {
		c := agenttest.ContainerOf(t, req, "geth")

		return c.Registry != nil && !c.Registry.Pulled && c.Registry.Digest == agenttest.GethV2
	})
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging geth, ask pk"}, req.Hold)
	assert.Empty(t, req.Needs)

	// The controller cannot lift a local hold, and cannot work around it.
	e.Ctrl.Direct(
		protocol.Directive{ID: "d-release", Kind: protocol.DirectiveReleaseHold},
		protocol.Directive{ID: "d-reboot", Kind: protocol.DirectiveReboot},
		protocol.Directive{ID: "d-roll", Kind: protocol.DirectiveRoll, Container: "geth"},
	)

	req = e.Ctrl.Await("directives answered", func(req *protocol.SyncRequest) bool {
		return agenttest.AckOf(req, "d-release") != nil && agenttest.AckOf(req, "d-reboot") != nil && agenttest.AckOf(req, "d-roll") != nil
	})
	assert.True(t, req.Hold.Local, "release-hold does not lift the local hold")
	assert.Contains(t, e.ReadOverrides(), "  local:\n", "the local hold stays in the file")
	assert.Equal(t, protocol.DirectiveAccepted, agenttest.AckOf(&req, "d-release").Status)
	assert.Equal(t, protocol.DirectiveRejected, agenttest.AckOf(&req, "d-reboot").Status)
	assert.Contains(t, agenttest.AckOf(&req, "d-reboot").Error, "local hold: debugging geth, ask pk")
	assert.Equal(t, protocol.DirectiveRejected, agenttest.AckOf(&req, "d-roll").Status)
	assert.Empty(t, req.Needs)
	e.Ctrl.Direct()

	// A lease is refused without touching anything.
	need := agenttest.ImageNeedFor("geth", agenttest.GethImage, agenttest.GethV1, agenttest.GethV2)
	e.Ctrl.Grant(e.Lease("lease-held", need, agenttest.RebootNeed()))

	op := e.Ctrl.AwaitFinished("lease-held")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepRefused, s.Status)
		assert.Contains(t, s.Error, "local hold")
	}

	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()))

	// Lifted on the host, the needs come back by themselves.
	e.WriteOverrides("hold: {}\n")

	req = e.Ctrl.Await("needs after the hold", func(req *protocol.SyncRequest) bool { return len(req.Needs) == 2 })
	assert.False(t, req.Hold.Active())
	assert.Equal(t, []string{need.ID, protocol.RebootNeedID}, agenttest.NeedIDs(&req))
}

func TestControllerHold(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := agenttest.NewEnv(t, sc)
	stop := e.Start()

	e.Ctrl.Await("reboot need", func(req *protocol.SyncRequest) bool { return agenttest.HasNeed(req, protocol.RebootNeedID) })

	e.Ctrl.Direct(protocol.Directive{
		ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "maintenance",
		By: &protocol.Actor{User: "sam@example.org", Feature: "staff"},
	})

	req := e.Ctrl.Await("held", func(req *protocol.SyncRequest) bool { return req.Hold.Controller })
	assert.False(t, req.Hold.Local)
	assert.Empty(t, req.Needs)
	assert.Contains(t, e.ReadOverrides(),
		"  controller:\n    by: \"sam@example.org\"\n    reason: \"maintenance\"\n    feature: \"staff\"\n",
		"the controller hold is an entry of the overrides file, with who asked")

	// The hold outlives the directive and the agent process.
	e.Ctrl.Direct()
	stop()
	e.Start()

	req = e.Ctrl.AwaitSyncs(3)
	assert.True(t, req.Hold.Controller, "the controller hold is persisted")
	assert.Empty(t, req.Needs)

	e.Ctrl.Grant(e.Lease("lease-held", agenttest.RebootNeed()))

	op := e.Ctrl.AwaitFinished("lease-held")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, op.Steps[0].Status)
	assert.Contains(t, op.Steps[0].Error, "controller hold")
	assert.Empty(t, e.Host.Calls())

	e.Ctrl.Direct(protocol.Directive{ID: "d-release", Kind: protocol.DirectiveReleaseHold})

	req = e.Ctrl.Await("released", func(req *protocol.SyncRequest) bool {
		return !req.Hold.Active() && agenttest.HasNeed(req, protocol.RebootNeedID)
	})
	assert.Equal(t, "boot", req.BootID, "releasing a hold does not reboot by itself")
	assert.NotContains(t, e.ReadOverrides(), "  controller:\n")

	// A hold again, then a person takes it out of the file by hand: the
	// agent reports it like any other change, and the controller decides.
	e.Ctrl.Direct(protocol.Directive{ID: "d-hold-2", Kind: protocol.DirectiveHold, Reason: "again"})
	e.Ctrl.Await("held again", func(req *protocol.SyncRequest) bool { return req.Hold.Controller })
	e.Ctrl.Direct()
	e.WriteOverrides("hold: {}\n")
	e.Ctrl.Await("the hand edit is reported", func(req *protocol.SyncRequest) bool { return !req.Hold.Active() })
}

func TestControllerDowntimeIsTolerated(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.RebootRequired = true

	e := agenttest.NewEnv(t, sc)
	e.Ctrl.Set(func(c *agenttest.Controller) { c.Down = true })
	e.Start()

	// Let the agent run into the wall a few times.
	time.Sleep(60 * time.Millisecond)
	require.Zero(t, e.Ctrl.SyncCount())

	e.Ctrl.Set(func(c *agenttest.Controller) { c.Down = false })

	req := e.Ctrl.Await("sync after the outage", func(req *protocol.SyncRequest) bool {
		return agenttest.HasNeed(req, protocol.RebootNeedID)
	})
	assert.Equal(t, "boot", req.BootID)
}

func TestInventoryIsSentWhenAsked(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
	sc.Packages.Upgradable = agenttest.DockerUpgrade()

	e := agenttest.NewEnv(t, sc)
	e.Start()

	req := e.Ctrl.AwaitSyncs(2)

	assert.Empty(t, e.Ctrl.Inventories(), "no inventory unless asked")
	e.Ctrl.Set(func(c *agenttest.Controller) { c.WantInventory = true })

	require.Eventually(t, func() bool {
		return len(e.Ctrl.Inventories()) > 0
	}, agenttest.AwaitTimeout, 2*time.Millisecond)

	inv := e.Ctrl.Inventories()[0]
	e.Ctrl.Set(func(c *agenttest.Controller) { c.WantInventory = false })

	hash, err := inv.ComputeHash()
	require.NoError(t, err)
	assert.Equal(t, hash, inv.Hash)
	assert.Equal(t, req.InventoryHash, inv.Hash, "the sync announces the hash of this inventory")
	assert.Len(t, inv.Packages, 3)
	assert.Len(t, inv.Upgradable, 2)
}

// The host's validator range is read from the marker file and goes out with
// the sync. (What a missing, damaged or changed marker gives is covered next
// to the reader.)
func TestSyncReportsTheValidatorRange(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())

	marker := filepath.Join(t.TempDir(), ".validator_keyrange")
	require.NoError(t, os.WriteFile(marker, []byte("0-64\n"), 0o600))

	e.Cfg.Validators.MarkerFile = marker
	e.Start()

	req := e.Ctrl.Await("the range", func(req *protocol.SyncRequest) bool {
		return req.Validators != nil && req.Validators.Known()
	})
	assert.Equal(t, &protocol.Validators{
		Start: 0, End: 64,
		Source: protocol.ValidatorsFromMarker, State: protocol.ValidatorsKnown,
	}, req.Validators)
}

// A lease that is cancelled before the agent has started it is never
// started. The agent says so at once, so the controller need not wait for
// the lease to run out.
func TestCancelledLeaseIsNotStarted(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Ctrl.Set(func(c *agenttest.Controller) { c.AutoRelease = false })
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	agenttest.GrantUnexamined(t, e, "docker daemon is restarting", e.Lease("lease-cancel", needs...))

	agenttest.Cancel(t, e)
	e.Host.Fail("observe", nil)

	op := e.Ctrl.AwaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, agenttest.StepOf(t, op, needs[0].ID).Status)
	assert.Contains(t, agenttest.StepOf(t, op, needs[0].ID).Error, "cancelled before the operation started")
	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()), "nothing was touched")
}

// A lease that the agent first hears of with the cancel mark is not taken.
func TestLeaseOfferedCancelledIsNotTaken(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Ctrl.Set(func(c *agenttest.Controller) { c.AutoRelease = false })
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))

	l := e.Lease("lease-cancel", needs...)
	l.Cancel = true
	e.Ctrl.Grant(l)
	e.Ctrl.AwaitSyncs(6)

	for _, s := range e.Ctrl.AllSyncs() {
		require.Nil(t, s.Operation)
	}

	assert.Nil(t, e.StateOnDisk()["current"])
	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()))
}

func TestLeaseIsPersistedBeforeActing(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")

	var (
		mu     sync.Mutex
		onDisk map[string]any
	)

	// What is on disk at the moment the first container is touched.
	e.Host.OnCall(func(call string) {
		mu.Lock()
		defer mu.Unlock()

		if onDisk == nil && strings.HasPrefix(call, "recreate ") {
			onDisk = e.StateOnDisk()
		}
	})

	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))
	lease := e.Lease("lease-1", needs...)
	lease.ExpiresAt = e.Now().Add(time.Hour)
	lease.TTLSeconds = 90

	granted := e.Now()
	e.Ctrl.Grant(lease)

	op := e.Ctrl.AwaitFinished("lease-1")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	mu.Lock()
	defer mu.Unlock()

	require.NotNil(t, onDisk, "state.json must exist when the container is recreated")

	// The lease is in the state file before anything is touched.
	assert.Equal(t, "lease-1", agenttest.Field(t, onDisk, "current", "lease", "id"))
	assert.Equal(t, true, agenttest.Field(t, onDisk, "current", "started"))
	assert.Equal(t, "applying", agenttest.Field(t, onDisk, "current", "operation", "phase"))

	// The deadline is the earlier of expiresAt (1h) and receipt + ttl (90s).
	text, ok := agenttest.Field(t, onDisk, "current", "deadline").(string)
	require.True(t, ok)

	deadline, err := time.Parse(time.RFC3339Nano, text)
	require.NoError(t, err)
	assert.WithinDuration(t, granted.Add(90*time.Second), deadline, 5*time.Second)
}

func TestTerminalOperationIsDroppedWhenTheLeaseDisappears(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Ctrl.Set(func(c *agenttest.Controller) { c.AutoRelease = false })
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))
	e.Ctrl.Grant(e.Lease("lease-1", needs...))

	e.Ctrl.AwaitFinished("lease-1")

	// While the controller restates the lease (it is waiting for its own
	// readiness check), the agent keeps reporting the finished operation
	// and does not run it again.
	seen := 0

	e.Ctrl.Await("ten more syncs", func(req *protocol.SyncRequest) bool {
		require.NotNil(t, req.Operation)
		require.Equal(t, "lease-1", req.Operation.LeaseID)
		require.Equal(t, protocol.PhaseDone, req.Operation.Phase)

		seen++

		return seen >= 10
	})

	_, ok := e.StateOnDisk()["current"]
	assert.True(t, ok, "the finished operation stays in the state file")

	// Released: the agent forgets it.
	e.Ctrl.Grant(nil)

	e.Ctrl.Await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })

	require.Eventually(t, func() bool {
		_, ok := e.StateOnDisk()["current"]

		return !ok
	}, agenttest.AwaitTimeout, 2*time.Millisecond)

	recreates := 0

	for _, c := range e.Host.Calls() {
		if strings.HasPrefix(c, "recreate ") {
			recreates++
		}
	}

	assert.Equal(t, 1, recreates)
}

// The controller stops restating the lease in the answer to a sync that was
// sent while the operation still ran, and the operation ends before that
// answer arrives. The agent must not forget an outcome it never reported.
func TestOperationIsNotForgottenBeforeItWasReported(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Ctrl.Set(func(c *agenttest.Controller) { c.AutoRelease = false })
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")

	entered, letGo := agenttest.BlockIn(e, "recreate geth")

	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))
	e.Ctrl.Grant(e.Lease("lease-1", needs...))

	agenttest.AwaitParked(t, entered, "recreate geth")

	var gateOnce sync.Once

	e.Ctrl.Set(func(c *agenttest.Controller) {
		c.Gate = func(req *protocol.SyncRequest) {
			if req.Operation == nil || req.Operation.Phase.Terminal() {
				return
			}

			gateOnce.Do(func() {
				// This sync reported a running operation. Drop the lease,
				// let the operation finish, and only then answer.
				e.Ctrl.Grant(nil)
				letGo()

				assert.Eventually(t, func() bool {
					doc := e.StateOnDisk()
					if doc == nil {
						return false
					}

					cur, _ := doc["current"].(map[string]any)
					op, _ := cur["operation"].(map[string]any)

					return op["phase"] == "done"
				}, agenttest.AwaitTimeout, time.Millisecond)
			})
		}
	})

	op := e.Ctrl.AwaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op.Phase, "the outcome is reported before it is forgotten")

	e.Ctrl.Await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })
}

func TestAnotherLeaseWaitsForTheCurrentOperation(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Ctrl.Set(func(c *agenttest.Controller) { c.AutoRelease = false })
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")

	// The first operation hangs in its recreate until the test lets go.
	entered, release := agenttest.BlockIn(e, "recreate geth")

	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2), protocol.ImageNeedID("teku", agenttest.TekuV2))
	e.Ctrl.Grant(e.Lease("lease-1", needs[0]))

	agenttest.AwaitParked(t, entered, "recreate geth")

	// The controller changes its mind in the middle of it.
	e.Ctrl.Grant(e.Lease("lease-2", needs[1]))

	seen := 0

	e.Ctrl.Await("syncs while lease-1 runs", func(req *protocol.SyncRequest) bool {
		require.NotNil(t, req.Operation, "the loop keeps syncing while the operation runs")
		require.Equal(t, "lease-1", req.Operation.LeaseID)
		require.Equal(t, protocol.PhaseApplying, req.Operation.Phase)

		seen++

		return seen >= 10
	})

	assert.Equal(t, []string{"recreate geth " + agenttest.GethV2 + " start=true"}, agenttest.Disruptive(e.Host.Calls()),
		"the second lease is not started while the first is open")

	release()

	// The first one finishes and is reported; only then the second runs.
	op1 := e.Ctrl.AwaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op1.Phase, op1.Error)

	op2 := e.Ctrl.AwaitFinished("lease-2")
	assert.Equal(t, protocol.PhaseDone, op2.Phase, op2.Error)

	assert.Equal(t, []string{
		"recreate geth " + agenttest.GethV2 + " start=true",
		"start geth", "start teku", "start validator",
		"recreate teku " + agenttest.TekuV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, agenttest.Disruptive(e.Host.Calls()))

	// Every sync that reported lease-2 came after lease-1 was terminal.
	terminal := false

	for _, s := range e.Ctrl.AllSyncs() {
		if s.Operation == nil {
			continue
		}

		switch s.Operation.LeaseID {
		case "lease-1":
			terminal = s.Operation.Phase.Terminal()
		case "lease-2":
			require.True(t, terminal)
		}
	}
}

// A lease that the controller stops offering before the agent has started it
// is not started any more: the controller has expired it, or a person has
// ended it, and a host must not go down under a lease that is over. Once
// started, an operation is finished whatever the controller offers
// (TestAnotherLeaseWaitsForTheCurrentOperation).
func TestLeaseWithdrawnBeforeItStartedIsNotStarted(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Ctrl.Set(func(c *agenttest.Controller) { c.AutoRelease = false })
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")
	e.Host.SetRegistry(agenttest.TekuImage, agenttest.TekuV2, "")
	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2), protocol.ImageNeedID("teku", agenttest.TekuV2))

	agenttest.GrantUnexamined(t, e, "docker daemon is restarting", e.Lease("lease-gone", needs[0]))

	// The controller no longer offers it.
	e.Ctrl.Set(func(c *agenttest.Controller) { c.Lease = nil })
	e.Ctrl.AwaitSyncs(3)

	// The host can be examined again, well before the lease's deadline.
	e.Host.Fail("observe", nil)

	op := e.Ctrl.AwaitFinished("lease-gone")

	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, agenttest.StepOf(t, op, needs[0].ID).Status)
	assert.Contains(t, agenttest.StepOf(t, op, needs[0].ID).Error, "no longer offered the lease")
	assert.Empty(t, agenttest.Disruptive(e.Host.Calls()), "nothing was touched")
	assert.Equal(t, agenttest.GethV1, e.Host.Container("geth").Digest)

	// The refusal reported, the agent is free for the lease that follows.
	e.Ctrl.Set(func(c *agenttest.Controller) { c.AutoRelease = true })
	e.Ctrl.Grant(e.Lease("lease-next", needs[1]))

	next := e.Ctrl.AwaitFinished("lease-next")
	assert.Equal(t, protocol.PhaseDone, next.Phase, next.Error)
	assert.Equal(t, agenttest.TekuV2, e.Host.Container("teku").Digest)
	assert.Equal(t, agenttest.GethV1, e.Host.Container("geth").Digest, "the withdrawn lease was never run")
}
