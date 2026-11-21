package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestNewValidatesItsInput(t *testing.T) {
	e := newEnv(t, baseScenario())

	_, err := New(e.log, e.cfg, e.creds, Deps{Runtime: e.host, Registry: e.host})
	require.Error(t, err, "a system is required")

	_, err = New(e.log, nil, e.creds, Deps{})
	require.Error(t, err)

	_, err = New(e.log, e.cfg, nil, Deps{Runtime: e.host, Registry: e.host, System: e.host})
	require.Error(t, err)

	raw := *e.cfg
	raw.Verify.Window = 0
	_, err = New(e.log, &raw, e.creds, Deps{Runtime: e.host, Registry: e.host, System: e.host})
	require.Error(t, err, "a configuration without defaults is refused")

	svc, err := New(e.log, e.cfg, e.creds, Deps{Runtime: e.host, Registry: e.host, System: e.host})
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
	e.cfg.Containers[0].Owners = []string{"panda", "geth"}
	e.cfg.Containers[1].Owners = []string{"teku"}
	require.NoError(t, e.cfg.Validate())
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
	for _, n := range req.Needs {
		assert.NotEqual(t, protocol.NeedSpec, n.Kind, "docker.io/x and x are the same reference")
	}
	require.NotNil(t, geth.Registry)
	assert.Equal(t, gethV1, geth.Registry.Digest)
	assert.True(t, geth.Registry.Pulled)
	assert.Empty(t, geth.Registry.Error)

	dora := containerOf(t, &req, "dora")
	assert.False(t, dora.Managed)
	assert.Empty(t, dora.Kind)
	assert.Nil(t, dora.Registry)

	// The owners of the configuration go out with each managed container,
	// sorted.
	assert.Equal(t, []string{"geth", "panda"}, geth.Owners)
	assert.Equal(t, []string{"teku"}, containerOf(t, &req, "teku").Owners)
	assert.Nil(t, containerOf(t, &req, "validator").Owners, "managed, nobody owns it")
	assert.Nil(t, dora.Owners, "not managed")

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

// A container that does not run what agent.yaml says (Ansible named a new
// image and left the restart to the agent, or somebody swapped it by hand)
// is reported with a spec need and recreated under a lease, its
// configuration kept, on the configured image at the digest its tag has.
func TestContainerIsRolledOntoItsSpec(t *testing.T) {
	e := newEnv(t, baseScenario())

	require.NoError(t, e.host.SetContainerImage("geth", "myfork/geth:debug", "sha256:debug-1"))
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	var need protocol.Need

	e.ctrl.await("a spec need", func(req *protocol.SyncRequest) bool {
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
	assert.Equal(t, gethV2, need.Spec.ToDigest)
	assert.Equal(t, "myfork/geth:debug", need.Spec.FromImage)
	assert.False(t, need.Spec.Flags)
	assert.False(t, need.Local, "hosts share the rollout of a spec")

	e.ctrl.grant(e.lease("lease-spec", need))

	op := e.ctrl.awaitFinished("lease-spec")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, need.ID).Status)
	assert.Equal(t, gethV2, e.host.Container("geth").Digest)
	assert.Equal(t, "docker.io/"+gethImage, e.host.Container("geth").Image)

	req := e.ctrl.await("in line with agent.yaml", func(req *protocol.SyncRequest) bool {
		return !hasNeed(req, need.ID)
	})
	assert.Empty(t, req.Needs)
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

// The host's validator range is read from the marker file and goes out with
// the sync. (What a missing, damaged or changed marker gives is covered next
// to the reader.)
func TestSyncReportsTheValidatorRange(t *testing.T) {
	e := newEnv(t, baseScenario())

	marker := filepath.Join(t.TempDir(), ".validator_keyrange")
	require.NoError(t, os.WriteFile(marker, []byte("0-64\n"), 0o600))

	e.cfg.Validators.MarkerFile = marker
	e.start()

	req := e.ctrl.await("the range", func(req *protocol.SyncRequest) bool {
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
	e := newEnv(t, baseScenario())
	e.ctrl.set(func(c *controller) { c.autoRelease = false })
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	grantUnexamined(t, e, "docker daemon is restarting", e.lease("lease-cancel", needs...))

	cancel(t, e)
	e.host.Fail("observe", nil)

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, stepOf(t, op, needs[0].ID).Status)
	assert.Contains(t, stepOf(t, op, needs[0].ID).Error, "cancelled before the operation started")
	assert.Empty(t, disruptive(e.host.Calls()), "nothing was touched")
}

// A lease that the agent first hears of with the cancel mark is not taken.
func TestLeaseOfferedCancelledIsNotTaken(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.ctrl.set(func(c *controller) { c.autoRelease = false })
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	l := e.lease("lease-cancel", needs...)
	l.Cancel = true
	e.ctrl.grant(l)
	e.ctrl.awaitSyncs(6)

	for _, s := range e.ctrl.allSyncs() {
		require.Nil(t, s.Operation)
	}

	assert.Nil(t, e.stateOnDisk()["current"])
	assert.Empty(t, disruptive(e.host.Calls()))
}

// inventorySystem is a hostapi.System of which only Inventory is used.
type inventorySystem struct {
	inventory func(ctx context.Context) (*protocol.Inventory, error)
}

func (s *inventorySystem) Status(context.Context) (*hostapi.SystemStatus, error) {
	return nil, errors.New("not used")
}

func (s *inventorySystem) Inventory(ctx context.Context) (*protocol.Inventory, error) {
	return s.inventory(ctx)
}

func (s *inventorySystem) UpgradePackages(context.Context, []string) error {
	return errors.New("not used")
}

func (s *inventorySystem) Reboot(context.Context) error { return errors.New("not used") }

// A package query that hangs must not stall the sync loop.
func TestInventoryCollectionIsBounded(t *testing.T) {
	log, logs := test.NewNullLogger()

	svc := &service{
		log: log,
		deps: Deps{System: &inventorySystem{
			inventory: func(ctx context.Context) (*protocol.Inventory, error) {
				<-ctx.Done()

				return nil, ctx.Err()
			},
		}},
		inventoryTimeout: 20 * time.Millisecond,
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		svc.sendInventory(t.Context())
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "sendInventory waits for a hanging inventory without a bound")
	}

	entry := logs.LastEntry()
	require.NotNil(t, entry)
	assert.Equal(t, "cannot collect the inventory", entry.Message)

	logged, ok := entry.Data[logrus.ErrorKey].(error)
	require.True(t, ok, "the error is logged")
	assert.ErrorIs(t, logged, context.DeadlineExceeded)
}

func TestLeaseDeadline(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		expires time.Time
		ttl     int64
		want    time.Time
	}{
		{name: "clocks agree", expires: now.Add(30 * time.Minute), ttl: 1800, want: now.Add(30 * time.Minute)},
		{name: "controller clock ahead", expires: now.Add(2 * time.Hour), ttl: 1800, want: now.Add(30 * time.Minute)},
		{name: "controller clock behind", expires: now.Add(-time.Hour), ttl: 1800, want: now.Add(-time.Hour)},
		{name: "restated late", expires: now.Add(10 * time.Minute), ttl: 600, want: now.Add(10 * time.Minute)},
		{name: "no ttl", expires: now.Add(time.Hour), ttl: 0, want: now},
		{name: "negative ttl", expires: now.Add(time.Hour), ttl: -5, want: now},
		{name: "no expiry", ttl: 60, want: now.Add(time.Minute)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := leaseDeadline(&protocol.Lease{ExpiresAt: tt.expires, TTLSeconds: tt.ttl}, now)
			assert.Equal(t, tt.want, got)
		})
	}
}
