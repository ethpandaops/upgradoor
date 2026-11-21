package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestLeaseIsPersistedBeforeActing(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")

	var (
		mu     sync.Mutex
		onDisk map[string]any
	)

	// What is on disk at the moment the first container is touched.
	e.host.OnCall(func(call string) {
		mu.Lock()
		defer mu.Unlock()

		if onDisk == nil && strings.HasPrefix(call, "recreate ") {
			onDisk = e.stateOnDisk()
		}
	})

	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))
	lease := e.lease("lease-1", needs...)
	lease.ExpiresAt = e.now().Add(time.Hour)
	lease.TTLSeconds = 90

	granted := e.now()
	e.ctrl.grant(lease)

	op := e.ctrl.awaitFinished("lease-1")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	mu.Lock()
	defer mu.Unlock()

	require.NotNil(t, onDisk, "state.json must exist when the container is recreated")

	// The lease is in the state file before anything is touched.
	assert.Equal(t, "lease-1", field(t, onDisk, "current", "lease", "id"))
	assert.Equal(t, true, field(t, onDisk, "current", "started"))
	assert.Equal(t, "applying", field(t, onDisk, "current", "operation", "phase"))

	// The deadline is the earlier of expiresAt (1h) and receipt + ttl (90s).
	text, ok := field(t, onDisk, "current", "deadline").(string)
	require.True(t, ok)

	deadline, err := time.Parse(time.RFC3339Nano, text)
	require.NoError(t, err)
	assert.WithinDuration(t, granted.Add(90*time.Second), deadline, 5*time.Second)
}

func TestExpiredLeaseNeverStarts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		expires time.Duration
		ttl     int64
	}{
		{name: "expiresAt has passed", expires: -time.Second, ttl: 60},
		{name: "no lifetime left at receipt", expires: time.Hour, ttl: 0},
		// A controller clock far behind: expiresAt looks old although the
		// lease is fresh. The earlier bound wins, in either direction.
		{name: "controller clock behind", expires: -time.Hour, ttl: 1800},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sc := baseScenario()
			sc.RebootRequired = true

			e := newEnv(t, sc)
			e.host.SetRegistry(gethImage, gethV2, "")
			e.start()

			needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.RebootNeedID)

			lease := e.lease("lease-late", needs...)
			lease.ExpiresAt = e.now().Add(tt.expires)
			lease.TTLSeconds = tt.ttl
			e.ctrl.grant(lease)

			op := e.ctrl.awaitFinished("lease-late")
			assert.Equal(t, protocol.PhaseDone, op.Phase, "an expired lease is not a failure")
			assert.False(t, op.Disrupted)
			assert.Empty(t, op.Error)

			for _, s := range op.Steps {
				assert.Equal(t, protocol.StepRefused, s.Status)
				assert.Contains(t, s.Error, "deadline")
			}

			assert.Empty(t, disruptive(e.host.Calls()))
			assert.Equal(t, "boot", e.host.BootID())

			// The needs are still open and are restated.
			req := e.ctrl.await("operation forgotten", func(req *protocol.SyncRequest) bool {
				return req.Operation == nil
			})
			assert.Len(t, req.Needs, 2)
		})
	}
}

func TestDigestMismatchSupersedesWhenTheTagMovedOn(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	stale := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	// The tag moves again before the slot comes; the agent pulls the newer
	// image, so the local tag no longer is the digest of the old need.
	e.host.SetRegistry(gethImage, gethV3, "")
	awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV3))

	e.ctrl.grant(e.lease("lease-stale", stale...))

	op := e.ctrl.awaitFinished("lease-stale")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted, "a superseded step costs nothing")
	assert.Empty(t, op.Error)

	step := stepOf(t, op, stale[0].ID)
	assert.Equal(t, protocol.StepSuperseded, step.Status)
	assert.Contains(t, step.Error, gethV3)

	assert.Empty(t, disruptive(e.host.Calls()))
	assert.Equal(t, gethV1, e.host.Container("geth").Digest)

	// The need for the newer digest is still open.
	req := e.ctrl.await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })
	assert.Equal(t, []string{protocol.ImageNeedID("geth", gethV3)}, needIDs(&req))
}

func TestDigestMismatchFailsWhenTheTagStillPointsThere(t *testing.T) {
	e := newEnv(t, baseScenario())

	// The registry has geth-2, but the image never arrives locally. A lease
	// for geth-2 must not make the agent run whatever is local.
	e.host.Fail("pull:"+gethImage, errors.New("no space left on device"))
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	e.ctrl.await("failed pull", func(req *protocol.SyncRequest) bool {
		c := containerOf(t, req, "geth")

		return c.Registry != nil && c.Registry.Error != ""
	})

	need := imageNeedFor("geth", gethImage, gethV1, gethV2)
	e.ctrl.grant(e.lease("lease-unpulled", need))

	op := e.ctrl.awaitFinished("lease-unpulled")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.False(t, op.Disrupted, "the digest is checked before anything stops")
	assert.Contains(t, op.Error, "although the tag still points at it")

	step := stepOf(t, op, need.ID)
	assert.Equal(t, protocol.StepFailed, step.Status)
	assert.Equal(t, op.Error, step.Error)

	assert.Empty(t, disruptive(e.host.Calls()))
	assert.Equal(t, gethV1, e.host.Container("geth").Digest)
	assert.Equal(t, protocol.StateRunning, e.host.Container("geth").State)
}

func TestRollOrder(t *testing.T) {
	sc := baseScenario()
	sc.Packages.Upgradable = dockerUpgrade()

	e := newEnv(t, sc)
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")
	e.host.SetRegistry(vcImage, vcV2, "")
	e.start()

	// Granted in an order that is not the order of execution.
	needs := awaitNeeds(t, e,
		protocol.ImageNeedID("validator", vcV2),
		protocol.ImageNeedID("teku", tekuV2),
		packagesNeed().ID,
		protocol.ImageNeedID("geth", gethV2),
	)
	e.ctrl.grant(e.lease("lease-all", needs...))

	op := e.ctrl.awaitFinished("lease-all")
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
		"recreate geth " + gethV2 + " start=true",
		"recreate teku " + tekuV2 + " start=true",
		"recreate validator " + vcV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, disruptive(e.host.Calls()))

	assert.Equal(t, gethV2, e.host.Container("geth").Digest)
	assert.Equal(t, "sha256:dora-1", e.host.Container("dora").Digest)

	req := e.ctrl.await("all served", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Zero(t, req.Pending.Blocked)

	// The phases were reported while it ran, in order and never backwards.
	assertPhaseOrder(t, e, "lease-all", protocol.PhaseDone)
}

func TestRebootOrder(t *testing.T) {
	sc := baseScenario()
	sc.Packages.Upgradable = dockerUpgrade()
	sc.RebootRequired = true
	sc.NewestKernel = "6.12.48-amd64"

	e := newEnv(t, sc)
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")
	e.start()

	needs := awaitNeeds(t, e,
		protocol.ImageNeedID("teku", tekuV2),
		protocol.RebootNeedID,
		protocol.ImageNeedID("geth", gethV2),
		packagesNeed().ID,
	)
	e.ctrl.grant(e.lease("lease-reboot", needs...))

	op := e.ctrl.awaitFinished("lease-reboot")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepDone, s.Status, s.NeedID)
	}

	// One downtime: drain VC, CL, EL; upgrade; recreate without starting;
	// reboot; start EL, CL, VC. The unmanaged container is left alone.
	assert.Equal(t, []string{
		"stop validator", "stop teku", "stop geth",
		"upgrade docker-ce",
		"recreate geth " + gethV2 + " start=false",
		"recreate teku " + tekuV2 + " start=false",
		"reboot",
		"start geth", "start teku", "start validator",
	}, disruptive(e.host.Calls()))

	assert.Equal(t, "boot-1", e.host.BootID())

	for _, name := range []string{"geth", "teku", "validator", "dora"} {
		assert.Equal(t, protocol.StateRunning, e.host.Container(name).State, name)
	}

	req := e.ctrl.await("all served", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Equal(t, "boot-1", req.BootID)
	assert.False(t, req.OS.RebootRequired)

	assertPhaseOrder(t, e, "lease-reboot", protocol.PhaseDone)
}

// assertPhaseOrder checks the phases reported for a lease: they never go
// backwards and end with last. (Which phases a sync happens to catch
// depends on timing; the fake host is instant.)
func assertPhaseOrder(t *testing.T, e *env, leaseID string, last protocol.Phase) {
	t.Helper()

	order := []protocol.Phase{
		protocol.PhaseDraining, protocol.PhaseApplying, protocol.PhaseRebooting,
		protocol.PhaseStarting, protocol.PhaseVerifying, protocol.PhaseDone, protocol.PhaseFailed,
	}
	rank := func(p protocol.Phase) int {
		for i, o := range order {
			if o == p {
				return i
			}
		}

		require.FailNow(t, "unknown phase reported", string(p))

		return -1
	}

	var seen []protocol.Phase

	for _, s := range e.ctrl.allSyncs() {
		if s.Operation == nil || s.Operation.LeaseID != leaseID {
			continue
		}

		p := s.Operation.Phase
		if len(seen) == 0 || seen[len(seen)-1] != p {
			seen = append(seen, p)
		}
	}

	require.NotEmpty(t, seen)
	assert.Equal(t, last, seen[len(seen)-1], "phases: %v", seen)

	for i := 1; i < len(seen); i++ {
		assert.Greater(t, rank(seen[i]), rank(seen[i-1]), "phases: %v", seen)
	}
}

func TestRebootSurvivesTheAgentRestart(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	// The reboot is accepted but the "host" stays up, so that the test can
	// end the agent process the way a real reboot would.
	e.host.SetInertReboot(true)
	e.host.SetRegistry(gethImage, gethV2, "")
	stop := e.start()

	needs := awaitNeeds(t, e, protocol.RebootNeedID, protocol.ImageNeedID("geth", gethV2))
	e.ctrl.grant(e.lease("lease-reboot", needs...))

	req := e.ctrl.await("phase rebooting", func(req *protocol.SyncRequest) bool {
		return req.Operation != nil && req.Operation.Phase == protocol.PhaseRebooting
	})
	assert.Equal(t, protocol.StepRunning, stepOf(t, *req.Operation, protocol.RebootNeedID).Status)
	assert.Equal(t, protocol.StepDone, stepOf(t, *req.Operation, needs[1].ID).Status)
	assert.True(t, req.Operation.Disrupted)

	// The process dies with the host ...
	stop()

	assert.Equal(t, "rebooting", field(t, e.stateOnDisk(), "current", "operation", "phase"),
		"the phase is on disk before the reboot")
	assert.Equal(t, "boot", field(t, e.stateOnDisk(), "current", "bootId"))

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.NotEqual(t, protocol.StateRunning, e.host.Container(name).State, name+" is drained")
	}

	callsBefore := len(e.host.Calls())

	// ... the host boots ...
	e.host.SetInertReboot(false)
	require.NoError(t, e.host.Reboot(context.Background()))
	require.Equal(t, "boot-1", e.host.BootID())

	// ... and a new agent process finds the operation in its state file.
	e.start()

	op := e.ctrl.awaitFinished("lease-reboot")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, protocol.RebootNeedID).Status)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[1].ID).Status)

	// After the boot the agent only starts containers, EL first. It does
	// not reboot again (the one "reboot" here is the test's own).
	assert.Equal(t, []string{"reboot", "start geth", "start teku", "start validator"},
		e.host.Calls()[callsBefore:])

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.Equal(t, protocol.StateRunning, e.host.Container(name).State, name)
	}

	assert.Equal(t, gethV2, e.host.Container("geth").Digest)

	req = e.ctrl.await("all served", func(req *protocol.SyncRequest) bool {
		return req.Operation == nil && len(req.Needs) == 0
	})
	assert.Equal(t, "boot-1", req.BootID)
}

func TestRebootThatNeverHappensFails(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)
	e.host.SetInertReboot(true)
	stop := e.start()

	needs := awaitNeeds(t, e, protocol.RebootNeedID)
	e.ctrl.grant(e.lease("lease-reboot", needs...))

	e.ctrl.await("phase rebooting", func(req *protocol.SyncRequest) bool {
		return req.Operation != nil && req.Operation.Phase == protocol.PhaseRebooting
	})

	// Shortly after the reboot was issued an unchanged boot id is normal:
	// the agent keeps waiting.
	req := e.ctrl.awaitSyncs(5)
	require.Equal(t, protocol.PhaseRebooting, req.Operation.Phase)

	stop()

	callsBefore := len(e.host.Calls())

	// Ten minutes later the agent runs again and the boot id still is the
	// same: the host did not reboot.
	e.clockOffset.Store(int64(10 * time.Minute))
	e.start()

	op := e.ctrl.awaitFinished("lease-reboot")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Contains(t, op.Error, "did not reboot")
	assert.Contains(t, op.Error, "boot id unchanged")
	assert.True(t, op.Disrupted)
	assert.Equal(t, protocol.StepFailed, stepOf(t, op, protocol.RebootNeedID).Status)

	// The drained containers are brought back all the same, and there is
	// no second reboot attempt.
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, e.host.Calls()[callsBefore:])
	assert.Equal(t, "boot", e.host.BootID())

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.Equal(t, protocol.StateRunning, e.host.Container(name).State, name)
	}
}

func TestFailedStepStillRestartsContainers(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")
	e.host.SetRegistry(vcImage, vcV2, "")
	e.host.Fail("recreate:teku", errors.New("port is already allocated"))
	e.start()

	needs := awaitNeeds(t, e,
		protocol.ImageNeedID("validator", vcV2),
		protocol.ImageNeedID("teku", tekuV2),
		protocol.ImageNeedID("geth", gethV2),
	)
	e.ctrl.grant(e.lease("lease-broken", needs...))

	op := e.ctrl.awaitFinished("lease-broken")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.True(t, op.Disrupted)
	assert.Contains(t, op.Error, "recreate container teku")
	assert.Contains(t, op.Error, "port is already allocated")

	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[2].ID).Status, "geth came first and worked")

	teku := stepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepFailed, teku.Status)
	assert.Contains(t, teku.Error, "port is already allocated")

	vc := stepOf(t, op, needs[0].ID)
	assert.Equal(t, protocol.StepPending, vc.Status, "nothing is attempted after a failure")
	assert.Contains(t, vc.Error, "not attempted")

	// The broken recreate left teku stopped; the agent starts everything
	// that ran before, and does not roll anything back.
	assert.Equal(t, []string{
		"recreate geth " + gethV2 + " start=true",
		"recreate teku " + tekuV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, disruptive(e.host.Calls()))

	assert.Equal(t, protocol.StateRunning, e.host.Container("teku").State)
	assert.Equal(t, gethV2, e.host.Container("geth").Digest, "no rollback")
	assert.Equal(t, vcV1, e.host.Container("validator").Digest)

	assertPhaseOrder(t, e, "lease-broken", protocol.PhaseFailed)
}

func TestVerifyingCatchesARestartingContainer(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.cfg.Verify.Window = 80 * time.Millisecond
	e.cfg.Verify.Timeout = 400 * time.Millisecond
	e.host.SetRegistry(gethImage, gethV2, "")

	// The new geth crash-loops as soon as it is created.
	var (
		once sync.Once
		done = make(chan struct{})
		wg   sync.WaitGroup
	)

	t.Cleanup(func() {
		close(done)
		wg.Wait()
	})

	e.host.OnCall(func(call string) {
		if !strings.HasPrefix(call, "recreate geth") {
			return
		}

		once.Do(func() {
			wg.Go(func() {
				for {
					select {
					case <-done:
						return
					case <-time.After(10 * time.Millisecond):
						_ = e.host.Crash("geth")
					}
				}
			})
		})
	})

	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))
	e.ctrl.grant(e.lease("lease-crashloop", needs...))

	op := e.ctrl.awaitFinished("lease-crashloop")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Contains(t, op.Error, "did not stay up")
	assert.Contains(t, op.Error, "geth restarted")
	// The recreate itself worked; what failed is the operation.
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[0].ID).Status)

	assertPhaseOrder(t, e, "lease-crashloop", protocol.PhaseFailed)
}

func TestLeaseCannotWidenWhatTheAgentDoes(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true
	sc.Packages.Upgradable = dockerUpgrade()

	e := newEnv(t, sc)
	e.cfg.Verbs = []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages}
	e.host.SetRegistry("ethpandaops/dora:latest", "sha256:dora-2", "")
	e.start()

	e.ctrl.awaitSyncs(2)

	openssl := []protocol.PackageChange{
		{Name: "docker-ce", From: "28.0", To: "28.1"},
		{Name: "openssl", From: "3.5.1", To: "3.5.2"},
	}
	needs := []protocol.Need{
		// reboot is not an allowed verb on this host
		rebootNeed(),
		// dora is not a managed container
		imageNeedFor("dora", "ethpandaops/dora:latest", "sha256:dora-1", "sha256:dora-2"),
		// openssl is not in the blocked set
		{
			ID: "packages/widened", Kind: protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: openssl},
		},
		// a kind outside the closed verb set
		{ID: "exec/1", Kind: "exec"},
		// a manual need the agent never created
		{
			ID: protocol.ManualNeedID("forged"), Kind: protocol.NeedImage, DirectiveID: "forged",
			Image: &protocol.ImageNeed{Container: "geth", Image: gethImage, FromDigest: gethV1, ToDigest: gethV1},
		},
		// geth already runs this digest
		imageNeedFor("geth", gethImage, "sha256:geth-0", gethV1),
		// teku is on another tag than the need says
		imageNeedFor("teku", "consensys/teku:latest", tekuV1, tekuV2),
	}
	e.ctrl.grant(e.lease("lease-wide", needs...))

	op := e.ctrl.awaitFinished("lease-wide")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)

	want := []struct {
		status protocol.StepStatus
		reason string
	}{
		{protocol.StepRefused, "verb reboot is not allowed"},
		{protocol.StepRefused, "dora is not managed"},
		{protocol.StepRefused, `"openssl" is not in the configured blocked set`},
		{protocol.StepRefused, "unknown need kind"},
		{protocol.StepSuperseded, "manual need is no longer open"},
		{protocol.StepSuperseded, "already runs the wanted digest"},
		{protocol.StepSuperseded, "the need was for consensys/teku:latest"},
	}

	require.Len(t, op.Steps, len(want))

	for i, w := range want {
		assert.Equal(t, w.status, op.Steps[i].Status, needs[i].ID)
		assert.Contains(t, op.Steps[i].Error, w.reason, needs[i].ID)
	}

	assert.Empty(t, e.host.Calls(), "nothing was touched, not even pulled")
	assert.Equal(t, "boot", e.host.BootID())
}

func TestTerminalOperationIsDroppedWhenTheLeaseDisappears(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.ctrl.set(func(c *controller) { c.autoRelease = false })
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))
	e.ctrl.grant(e.lease("lease-1", needs...))

	e.ctrl.awaitFinished("lease-1")

	// While the controller restates the lease (it is waiting for its own
	// readiness check), the agent keeps reporting the finished operation
	// and does not run it again.
	seen := 0

	e.ctrl.await("ten more syncs", func(req *protocol.SyncRequest) bool {
		require.NotNil(t, req.Operation)
		require.Equal(t, "lease-1", req.Operation.LeaseID)
		require.Equal(t, protocol.PhaseDone, req.Operation.Phase)

		seen++

		return seen >= 10
	})

	_, ok := e.stateOnDisk()["current"]
	assert.True(t, ok, "the finished operation stays in the state file")

	// Released: the agent forgets it.
	e.ctrl.grant(nil)

	e.ctrl.await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })

	require.Eventually(t, func() bool {
		_, ok := e.stateOnDisk()["current"]

		return !ok
	}, awaitTimeout, 2*time.Millisecond)

	recreates := 0

	for _, c := range e.host.Calls() {
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
	e := newEnv(t, baseScenario())
	e.ctrl.set(func(c *controller) { c.autoRelease = false })
	e.host.SetRegistry(gethImage, gethV2, "")

	entered, letGo := blockIn(e, "recreate geth")

	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))
	e.ctrl.grant(e.lease("lease-1", needs...))

	awaitParked(t, entered, "recreate geth")

	var gateOnce sync.Once

	e.ctrl.set(func(c *controller) {
		c.gate = func(req *protocol.SyncRequest) {
			if req.Operation == nil || req.Operation.Phase.Terminal() {
				return
			}

			gateOnce.Do(func() {
				// This sync reported a running operation. Drop the lease,
				// let the operation finish, and only then answer.
				e.ctrl.grant(nil)
				letGo()

				assert.Eventually(t, func() bool {
					doc := e.stateOnDisk()
					if doc == nil {
						return false
					}

					cur, _ := doc["current"].(map[string]any)
					op, _ := cur["operation"].(map[string]any)

					return op["phase"] == "done"
				}, awaitTimeout, time.Millisecond)
			})
		}
	})

	op := e.ctrl.awaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op.Phase, "the outcome is reported before it is forgotten")

	e.ctrl.await("operation forgotten", func(req *protocol.SyncRequest) bool { return req.Operation == nil })
}

func TestAnotherLeaseWaitsForTheCurrentOperation(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.ctrl.set(func(c *controller) { c.autoRelease = false })
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")

	// The first operation hangs in its recreate until the test lets go.
	entered, release := blockIn(e, "recreate geth")

	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-1", needs[0]))

	awaitParked(t, entered, "recreate geth")

	// The controller changes its mind in the middle of it.
	e.ctrl.grant(e.lease("lease-2", needs[1]))

	seen := 0

	e.ctrl.await("syncs while lease-1 runs", func(req *protocol.SyncRequest) bool {
		require.NotNil(t, req.Operation, "the loop keeps syncing while the operation runs")
		require.Equal(t, "lease-1", req.Operation.LeaseID)
		require.Equal(t, protocol.PhaseApplying, req.Operation.Phase)

		seen++

		return seen >= 10
	})

	assert.Equal(t, []string{"recreate geth " + gethV2 + " start=true"}, disruptive(e.host.Calls()),
		"the second lease is not started while the first is open")

	release()

	// The first one finishes and is reported; only then the second runs.
	op1 := e.ctrl.awaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op1.Phase, op1.Error)

	op2 := e.ctrl.awaitFinished("lease-2")
	assert.Equal(t, protocol.PhaseDone, op2.Phase, op2.Error)

	assert.Equal(t, []string{
		"recreate geth " + gethV2 + " start=true",
		"start geth", "start teku", "start validator",
		"recreate teku " + tekuV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, disruptive(e.host.Calls()))

	// Every sync that reported lease-2 came after lease-1 was terminal.
	terminal := false

	for _, s := range e.ctrl.allSyncs() {
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

// A process that is killed (or a host that loses power) in the middle of a
// step leaves the phase applying behind; a regular stop never does.
func TestKilledOperationIsResumedAsFailed(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")

	// The first agent process dies inside the recreate of geth: the call
	// never returns to it. Its sync loop and watcher are ended; the
	// operation goroutine stays parked in the call until the test is over,
	// which is as dead as a goroutine gets.
	ctx, kill := context.WithCancel(t.Context())
	defer kill()

	inCall, release := blockIn(e, "recreate geth")

	dead := e.newAgent()
	require.NoError(t, dead.Start(ctx))

	t.Cleanup(func() {
		release()
		require.NoError(t, dead.Stop())
	})

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-cut", needs...))

	awaitParked(t, inCall, "recreate geth")
	kill()

	assert.Equal(t, "applying", field(t, e.stateOnDisk(), "current", "operation", "phase"))

	// Nobody knows what the cut-off step left behind. The next agent
	// process does not guess: it fails the step, brings the containers
	// back and reports.
	e.start()

	op := e.ctrl.awaitFinished("lease-cut")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Contains(t, op.Error, "the agent restarted during phase applying")
	assert.Equal(t, protocol.StepFailed, stepOf(t, op, needs[0].ID).Status)
	assert.Equal(t, protocol.StepPending, stepOf(t, op, needs[1].ID).Status)

	calls := disruptive(e.host.Calls())
	require.GreaterOrEqual(t, len(calls), 4)
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, calls[len(calls)-3:])
	assert.NotContains(t, strings.Join(calls, "\n"), "recreate teku")
}

// Stopping the agent (a restart of the unit, a redeployed binary) is not a
// crash: the step in flight and the steps after it are carried through,
// and the next process only has the waiting left.
func TestStopFinishesTheDisruptiveSteps(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")

	// The agent is told to stop right when the recreate of geth begins.
	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	var once sync.Once

	e.host.OnCall(func(call string) {
		if strings.HasPrefix(call, "recreate geth") {
			once.Do(sigterm)
		}
	})

	svc := e.newAgent()
	require.NoError(t, svc.Start(ctx))

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-stop", needs...))

	<-ctx.Done()
	require.NoError(t, svc.Stop())

	// Stop waited: both containers are recreated and running again. Only
	// the verifying wait was given up.
	assert.Equal(t, []string{
		"recreate geth " + gethV2 + " start=true",
		"recreate teku " + tekuV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, disruptive(e.host.Calls()))
	assert.Equal(t, gethV2, e.host.Container("geth").Digest)
	assert.Equal(t, tekuV2, e.host.Container("teku").Digest)
	assert.Equal(t, protocol.StateRunning, e.host.Container("teku").State)
	assert.Equal(t, "verifying", field(t, e.stateOnDisk(), "current", "operation", "phase"))

	e.host.OnCall(nil)
	e.start()

	op := e.ctrl.awaitFinished("lease-stop")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Empty(t, op.Error)

	for _, s := range op.Steps {
		assert.Equal(t, protocol.StepDone, s.Status, s.NeedID)
	}

	recreates := 0

	for _, call := range e.host.Calls() {
		if strings.HasPrefix(call, "recreate ") {
			recreates++
		}
	}

	assert.Equal(t, 2, recreates, "nothing is done twice")
}

// A stop during the drain must not leave the host drained and waiting: the
// reboot the drain was for is still issued.
func TestStopDuringTheDrainStillReboots(t *testing.T) {
	sc := baseScenario()
	sc.RebootRequired = true

	e := newEnv(t, sc)

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	var once sync.Once

	e.host.OnCall(func(call string) {
		if call == "stop validator" {
			once.Do(sigterm)
		}
	})

	svc := e.newAgent()
	require.NoError(t, svc.Start(ctx))

	needs := awaitNeeds(t, e, protocol.RebootNeedID)
	e.ctrl.grant(e.lease("lease-drain", needs...))

	<-ctx.Done()
	require.NoError(t, svc.Stop())

	assert.Equal(t, []string{"stop validator", "stop teku", "stop geth", "reboot"},
		disruptive(e.host.Calls()))
	assert.Equal(t, "boot-1", e.host.BootID())
	assert.Equal(t, "rebooting", field(t, e.stateOnDisk(), "current", "operation", "phase"))

	e.host.OnCall(nil)
	e.start()

	op := e.ctrl.awaitFinished("lease-drain")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, protocol.RebootNeedID).Status)

	calls := disruptive(e.host.Calls())
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, calls[len(calls)-3:])
}

// A Docker daemon that does not answer when the lease arrives is no reason
// to report a failed operation, which would halt the rollout: the agent
// looks again for as long as the lease is good.
func TestHostThatCannotBeExaminedDoesNotFailTheLease(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	grantUnexamined(t, e, "docker daemon is restarting", e.lease("lease-retry", needs...))
	e.ctrl.awaitSyncs(8)

	for _, s := range e.ctrl.allSyncs() {
		require.Nil(t, s.Operation, "nothing is decided while the host cannot be examined")
	}

	assert.Empty(t, disruptive(e.host.Calls()))

	e.host.Fail("observe", nil)

	op := e.ctrl.awaitFinished("lease-retry")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[0].ID).Status)
	assert.Equal(t, gethV2, e.host.Container("geth").Digest)
}

// If the host cannot be examined until the lease has run out, the lease
// was never started. That is not a failure either.
func TestLeaseRunsOutWhileTheHostCannotBeExamined(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	// Let the agent look (in vain) while the lease is still good, then let
	// the lease run out.
	grantUnexamined(t, e, "docker daemon is gone", e.lease("lease-blind", needs...))
	e.ctrl.awaitSyncs(8)
	e.clockOffset.Store(int64(2 * time.Minute))

	op := e.ctrl.awaitFinished("lease-blind")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Empty(t, op.Error)

	step := stepOf(t, op, needs[0].ID)
	assert.Equal(t, protocol.StepRefused, step.Status)
	assert.Contains(t, step.Error, "before the host could be examined")
	assert.Contains(t, step.Error, "docker daemon is gone")
	assert.Empty(t, disruptive(e.host.Calls()))
	assert.Equal(t, gethV1, e.host.Container("geth").Digest)
}

// A lease that the controller stops offering before the agent has started it
// is not started any more: the controller has expired it, or a person has
// ended it, and a host must not go down under a lease that is over. Once
// started, an operation is finished whatever the controller offers
// (TestAnotherLeaseWaitsForTheCurrentOperation).
func TestLeaseWithdrawnBeforeItStartedIsNotStarted(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.ctrl.set(func(c *controller) { c.autoRelease = false })
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))

	grantUnexamined(t, e, "docker daemon is restarting", e.lease("lease-gone", needs[0]))

	// The controller no longer offers it.
	e.ctrl.set(func(c *controller) { c.lease = nil })
	e.ctrl.awaitSyncs(3)

	// The host can be examined again, well before the lease's deadline.
	e.host.Fail("observe", nil)

	op := e.ctrl.awaitFinished("lease-gone")

	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, stepOf(t, op, needs[0].ID).Status)
	assert.Contains(t, stepOf(t, op, needs[0].ID).Error, "no longer offered the lease")
	assert.Empty(t, disruptive(e.host.Calls()), "nothing was touched")
	assert.Equal(t, gethV1, e.host.Container("geth").Digest)

	// The refusal reported, the agent is free for the lease that follows.
	e.ctrl.set(func(c *controller) { c.autoRelease = true })
	e.ctrl.grant(e.lease("lease-next", needs[1]))

	next := e.ctrl.awaitFinished("lease-next")
	assert.Equal(t, protocol.PhaseDone, next.Phase, next.Error)
	assert.Equal(t, tekuV2, e.host.Container("teku").Digest)
	assert.Equal(t, gethV1, e.host.Container("geth").Digest, "the withdrawn lease was never run")
}

// A container that crashed on a broken image must come back when a newer
// image is rolled onto it; one that somebody stopped must stay down.
func TestRollStartsACrashedContainerButNotAStoppedOne(t *testing.T) {
	e := newEnv(t, baseScenario())

	// teku died by itself; the validator was stopped by a person.
	require.NoError(t, e.host.Exit("teku", 1))
	require.NoError(t, e.host.Exit("validator", 143))

	e.host.SetRegistry(tekuImage, tekuV2, "")
	e.host.SetRegistry(vcImage, vcV2, "")
	e.start()

	needs := awaitNeeds(t, e,
		protocol.ImageNeedID("teku", tekuV2),
		protocol.ImageNeedID("validator", vcV2),
	)
	e.ctrl.grant(e.lease("lease-repair", needs...))

	op := e.ctrl.awaitFinished("lease-repair")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	calls := disruptive(e.host.Calls())
	assert.Contains(t, calls, "recreate teku "+tekuV2+" start=true")
	assert.Contains(t, calls, "recreate validator "+vcV2+" start=false")
	assert.NotContains(t, calls, "start validator")

	assert.Equal(t, protocol.StateRunning, e.host.Container("teku").State)
	assert.Equal(t, "created", e.host.Container("validator").State, "recreated, not started")
	assert.Equal(t, vcV2, e.host.Container("validator").Digest, "rolled all the same")
}

// The cancel outlives the agent process. Two images; the lease is ended
// while the first is recreated, and the agent is stopped while it verifies.
// The process that takes the operation up from the state file reports the
// step that was never begun as cancelled, not as pending.
func TestCancelIsRememberedAcrossAnAgentRestart(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")

	entered, release := blockIn(e, "recreate geth")

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	svc := e.newAgent()
	require.NoError(t, svc.Start(ctx))

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, "recreate geth")
	cancel(t, e)

	// The agent is told to stop, and the recreate ends. Stop waits for the
	// step in progress and for the containers to be started; the verifying
	// wait is given up.
	sigterm()
	release()
	require.NoError(t, svc.Stop())

	require.Equal(t, "verifying", field(t, e.stateOnDisk(), "current", "operation", "phase"))
	require.Equal(t, true, field(t, e.stateOnDisk(), "current", "cancelled"))
	assert.NotContains(t, strings.Join(disruptive(e.host.Calls()), "\n"), "recreate teku")

	// A new process finds the operation in its state file.
	e.start()

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[0].ID).Status)

	step := stepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepCancelled, step.Status, "not begun because of the cancel, also after a restart")
	assert.Contains(t, step.Error, "cancelled")
	assert.NotContains(t, strings.Join(disruptive(e.host.Calls()), "\n"), "recreate teku")
	assert.Equal(t, tekuV1, e.host.Container("teku").Digest)
	assertAllRunning(t, e)
}

// A person ends a lease while the agent is inside a disruptive call. That
// call is completed and counts as done; nothing else is begun, the steps
// that had not begun are reported cancelled, and what was stopped is started
// again by the path that brings a host back after a failed step.
func TestCancelDuringAnOperation(t *testing.T) {
	const cancelled, done = protocol.StepCancelled, protocol.StepDone

	var (
		gethNeed = protocol.ImageNeedID("geth", gethV2)
		tekuNeed = protocol.ImageNeedID("teku", tekuV2)
		starts   = []string{"start geth", "start teku", "start validator"}
	)

	tests := []struct {
		name string
		// images are the tags that have moved; packages and reboot are the
		// other needs of the host.
		images   map[string]string
		packages bool
		reboot   bool
		// parkAt is the call the agent is inside when the cancel arrives.
		parkAt string
		needs  []string
		want   []protocol.StepStatus
		// geth is the digest of the execution client afterwards.
		geth  string
		check func(t *testing.T, calls []string)
	}{
		{
			// Two images and a reboot. The first disruptive call is the stop
			// of the validator client ahead of the reboot: no other container
			// is stopped, none is recreated, the host is not rebooted.
			name:   "in the first stop of the drain",
			images: map[string]string{gethImage: gethV2, tekuImage: tekuV2},
			reboot: true,
			parkAt: "stop validator",
			needs:  []string{gethNeed, tekuNeed, protocol.RebootNeedID},
			want:   []protocol.StepStatus{cancelled, cancelled, cancelled},
			geth:   gethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				assert.Equal(t, starts, callsAfter(calls, "stop validator"),
					"after the call in progress the host is only brought back")
			},
		},
		{
			// Without a reboot the first disruptive call is the recreate of
			// the execution client; the consensus client is left alone.
			name:   "in a recreate",
			images: map[string]string{gethImage: gethV2, tekuImage: tekuV2},
			parkAt: "recreate geth",
			needs:  []string{gethNeed, tekuNeed},
			want:   []protocol.StepStatus{done, cancelled},
			geth:   gethV2,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				joined := strings.Join(calls, "\n")
				assert.NotContains(t, joined, "recreate teku")
				assert.NotContains(t, joined, "stop ")
				assert.NotContains(t, joined, "reboot")
			},
		},
		{
			// The upgrade ends; the recreate that follows it is not begun.
			name:     "in the package upgrade, ahead of a recreate",
			images:   map[string]string{gethImage: gethV2},
			packages: true,
			parkAt:   "upgrade ",
			needs:    []string{packagesNeed().ID, gethNeed},
			want:     []protocol.StepStatus{done, cancelled},
			geth:     gethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				assert.NotContains(t, strings.Join(calls, "\n"), "recreate")
			},
		},
		{
			// The host is drained. The upgrade ends; the reboot, which had
			// not begun, is not issued (operation.reboot asks under the state
			// lock), and the drained containers are started again.
			name:     "in the package upgrade, ahead of the reboot",
			packages: true,
			reboot:   true,
			parkAt:   "upgrade ",
			needs:    []string{packagesNeed().ID, protocol.RebootNeedID},
			want:     []protocol.StepStatus{done, cancelled},
			geth:     gethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				assert.NotContains(t, calls, "reboot")
				assert.Equal(t, starts, callsAfter(calls, "upgrade "),
					"after the upgrade the host is only brought back")
			},
		},
		{
			// The execution client is the last container the drain stops.
			// That stop is completed; the upgrade is not begun
			// (operation.applyPackages asks first), nor is the reboot.
			name:     "between the drain and the package upgrade",
			packages: true,
			reboot:   true,
			parkAt:   "stop geth",
			needs:    []string{packagesNeed().ID, protocol.RebootNeedID},
			want:     []protocol.StepStatus{cancelled, cancelled},
			geth:     gethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				assert.Equal(t, append([]string{"stop validator", "stop teku", "stop geth"}, starts...),
					calls, "drained, then only brought back: no upgrade, no reboot")
			},
		},
		{
			// Nothing is left to keep from beginning: the operation ends as
			// it would have.
			name:   "after the last step has begun",
			images: map[string]string{gethImage: gethV2},
			parkAt: "recreate geth",
			needs:  []string{gethNeed},
			want:   []protocol.StepStatus{done},
			geth:   gethV2,
			check:  func(*testing.T, []string) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := baseScenario()
			sc.RebootRequired = tt.reboot

			e := newEnv(t, sc)

			for image, digest := range tt.images {
				e.host.SetRegistry(image, digest, "")
			}

			if tt.packages {
				e.host.SetUpgradable(dockerUpgrade())
			}

			entered, release := blockIn(e, tt.parkAt)

			e.start()

			needs := awaitNeeds(t, e, tt.needs...)
			e.ctrl.grant(e.lease("lease-cancel", needs...))

			awaitParked(t, entered, tt.parkAt)
			cancel(t, e)
			release()

			op := e.ctrl.awaitFinished("lease-cancel")

			assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
			assert.True(t, op.Disrupted, "the host was touched")

			for i, id := range tt.needs {
				step := stepOf(t, op, id)
				assert.Equal(t, tt.want[i], step.Status, id)

				if tt.want[i] == cancelled {
					assert.Contains(t, step.Error, "cancelled", id)
				}
			}

			tt.check(t, disruptive(e.host.Calls()))

			assertAllRunning(t, e)
			assert.Equal(t, tt.geth, e.host.Container("geth").Digest)
			assert.Equal(t, tekuV1, e.host.Container("teku").Digest)
			assert.Equal(t, "boot", e.host.BootID(), "no reboot")
		})
	}
}

// What the agent reports does not depend on whether it was restarted. The
// recreate of the first of two images fails and the lease is ended while it
// runs; the agent is stopped before it could finish (the start of the
// execution client keeps failing until then) and a new process ends the
// operation from the state file. The step that was never begun is reported
// as after any failure (pending, not attempted): it is reported as cancelled
// only when the cancel is what stopped the operation.
func TestReportAfterAFailedStepDoesNotDependOnARestart(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.host.SetRegistry(tekuImage, tekuV2, "")
	e.host.Fail("recreate:geth", errors.New("no space left on device"))
	e.host.Fail("start:geth", errors.New("docker daemon is restarting"))

	entered, release := blockIn(e, "recreate geth")

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	svc := e.newAgent()
	require.NoError(t, svc.Start(ctx))

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-cancel", needs...))

	awaitParked(t, entered, "recreate geth")
	cancel(t, e)

	sigterm()
	release()
	require.NoError(t, svc.Stop())

	require.Equal(t, "starting", field(t, e.stateOnDisk(), "current", "operation", "phase"))
	require.Equal(t, true, field(t, e.stateOnDisk(), "current", "cancelled"))

	e.host.Fail("start:geth", nil)
	e.start()

	op := e.ctrl.awaitFinished("lease-cancel")

	assert.NotContains(t, strings.Join(disruptive(e.host.Calls()), "\n"), "recreate teku")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Equal(t, protocol.StepFailed, stepOf(t, op, needs[0].ID).Status)

	step := stepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepPending, step.Status,
		"not begun because a step failed: reported as after any failure")
	assert.Contains(t, step.Error, "not attempted")
}

// steppedClock is a clock that only moves when it is told to.
type steppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *steppedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// tickingRuntime is a runtime with one container. Every call takes one
// second on the stepped clock. Start fails until startsAt; the container
// restarts on every look while flapping is set.
type tickingRuntime struct {
	clock    *steppedClock
	startsAt time.Time
	flapping bool
	restarts int
}

func (r *tickingRuntime) Observe(context.Context) ([]protocol.Container, error) {
	r.clock.Advance(time.Second)

	if r.flapping {
		r.restarts++
	}

	return []protocol.Container{
		{Name: "geth", State: protocol.StateRunning, RestartCount: r.restarts},
	}, nil
}

func (r *tickingRuntime) Start(context.Context, string) error {
	r.clock.Advance(time.Second)

	if r.clock.Now().Before(r.startsAt) {
		return errors.New("docker daemon is not up yet")
	}

	return nil
}

func (r *tickingRuntime) LocalImage(context.Context, string) (digest, revision string, err error) {
	return "", "", errors.New("not used")
}

func (r *tickingRuntime) Pull(context.Context, string) (digest, revision string, err error) {
	return "", "", errors.New("not used")
}

func (r *tickingRuntime) Recreate(context.Context, string, string, time.Duration, bool) error {
	return errors.New("not used")
}

func (r *tickingRuntime) RecreateAs(
	context.Context, string, string, time.Duration, bool, hostapi.Recreation,
) error {
	return errors.New("not used")
}

func (r *tickingRuntime) Stop(context.Context, string, time.Duration) error {
	return errors.New("not used")
}

// bringBackOperation is an operation that has disrupted geth and now has to
// start and verify it, on a stepped clock.
func bringBackOperation(t *testing.T, rt *tickingRuntime) *operation {
	t.Helper()

	store, err := openStateStore(quietLog(), t.TempDir())
	require.NoError(t, err)

	require.NoError(t, store.Update(func(st *persistedState) {
		st.Current = &operationRecord{
			Lease:   protocol.Lease{ID: "lease-1"},
			Started: true,
			Operation: protocol.Operation{
				LeaseID: "lease-1", Phase: protocol.PhaseApplying, Disrupted: true,
				StartedAt: rt.clock.Now(),
				Steps:     []protocol.Step{{NeedID: "image/geth/x", Status: protocol.StepDone}},
			},
			WasRunning: []string{"geth"},
			Touched:    []string{"geth"},
		}
	}))

	return &operation{
		log: quietLog(),
		cfg: &Config{
			Verify: VerifyConfig{Window: 10 * time.Second, Timeout: 100 * time.Second},
		},
		runtime: rt,
		store:   store,
		now:     rt.clock.Now,
		poll:    time.Millisecond,
		notify:  func() {},
	}
}

// verify.timeout bounds starting and verifying together, measured on the
// agent's clock.
func TestStartingAndVerifyingShareOneTimeout(t *testing.T) {
	clock := &steppedClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	began := clock.Now()

	// Starting takes 60 of the 100 seconds; the container then never stays
	// up. Verifying has the remaining 40 seconds, not another 100.
	rt := &tickingRuntime{clock: clock, startsAt: began.Add(60 * time.Second), flapping: true}
	op := bringBackOperation(t, rt)

	require.NoError(t, op.bringBack(t.Context()))

	rec := op.store.Snapshot().Current
	require.NotNil(t, rec)
	assert.Equal(t, protocol.PhaseFailed, rec.Operation.Phase)
	assert.Contains(t, rec.Operation.Error, "did not stay up")
	assert.Contains(t, rec.Operation.Error, "container geth restarted")

	took := clock.Now().Sub(began)
	assert.GreaterOrEqual(t, took, 100*time.Second)
	assert.LessOrEqual(t, took, 102*time.Second,
		"starting and verifying together took %s of a timeout of 100s", took)
}

// The window is measured on the agent's clock too: ten seconds of it pass
// here in a few milliseconds.
func TestVerifyWindowFollowsTheInjectedClock(t *testing.T) {
	clock := &steppedClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	began := clock.Now()

	op := bringBackOperation(t, &tickingRuntime{clock: clock, startsAt: began})

	require.NoError(t, op.bringBack(t.Context()))

	rec := op.store.Snapshot().Current
	require.NotNil(t, rec)
	assert.Equal(t, protocol.PhaseDone, rec.Operation.Phase, rec.Operation.Error)

	took := clock.Now().Sub(began)
	assert.GreaterOrEqual(t, took, 10*time.Second, "the container was watched for the window")
	assert.LessOrEqual(t, took, 13*time.Second)
}

// Whether a step has begun and whether the lease is cancelled are decided in
// one critical section of the state store: a cancel that the sync loop writes
// (service.cancelLease) either comes first, and proceed begins nothing, or
// comes after, and finds the step begun. Here a cancel and proceed race each
// other a few thousand times; the cancel notes, in its own critical section,
// whether the step had begun, and that is what proceed must have answered.
// A proceed that looked at the mark outside the section and began the step
// inside it would answer yes to a cancel that found the step not begun.
//
// The test cannot be made to fail on the first iteration: the interleaving
// that shows the fault lies between two lock acquisitions inside proceed,
// where a test has no handle that a wrong proceed would still pass through.
// What it does instead is race a few hundred times: several stores at once,
// each iteration with both sides released together. With the mark read
// outside the lock about one iteration in a hundred shows it, so a run can
// miss the fault; it costs about a second, and it runs on every change.
func TestProceedAndCancelExcludeEachOther(t *testing.T) {
	const (
		workers = 8
		races   = 50 // per worker
	)

	var (
		first atomic.Int64
		wg    sync.WaitGroup
	)

	for range workers {
		store, err := openStateStore(quietLog(), t.TempDir())
		require.NoError(t, err)

		// The durability of the rename is not what is tested here.
		store.syncDir = func(string) error { return nil }

		wg.Add(1)

		go func() {
			defer wg.Done()

			for range races {
				proceeded, ok := raceProceedAndCancel(t, store)
				if !ok {
					return
				}

				if proceeded {
					first.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	t.Logf("%d races, proceed came first in %d", workers*races, first.Load())
}

// raceProceedAndCancel lets one proceed and one cancel loose on a fresh
// operation and holds what each found against the other. It reports what
// proceed answered, and false when the two disagree.
func raceProceedAndCancel(t *testing.T, store *stateStore) (proceeded, ok bool) {
	t.Helper()

	ids := []string{"image/geth/x"}

	// A fresh operation, in memory only: the two writes that race are the
	// ones that count.
	store.mu.Lock()
	store.cur.Current = &operationRecord{
		Lease:   protocol.Lease{ID: "lease-1"},
		Started: true,
		Operation: protocol.Operation{
			LeaseID: "lease-1", Phase: protocol.PhaseApplying,
			Steps: []protocol.Step{{NeedID: ids[0], Status: protocol.StepPending}},
		},
	}
	store.mu.Unlock()

	op := &operation{log: quietLog(), store: store, now: time.Now, notify: func() {}}

	var (
		wg    sync.WaitGroup
		begun bool
		start = make(chan struct{})
	)

	wg.Add(2)

	go func() {
		defer wg.Done()
		<-start

		proceeded = op.proceed(ids)
	}()

	go func() {
		defer wg.Done()
		<-start

		// What service.cancelLease writes, and what it finds.
		assert.NoError(t, store.Apply(func(st *persistedState) {
			begun = st.Current.Operation.Steps[0].Status == protocol.StepRunning
			st.Current.Cancelled = true
		}))
	}()

	close(start)
	wg.Wait()

	rec := store.Snapshot().Current

	return proceeded, assert.True(t, rec.Cancelled) &&
		assert.Equal(t, begun, proceeded, "the cancel found the step begun: %v, proceed said: %v", begun, proceeded) &&
		assert.Equal(t, proceeded, rec.Operation.Steps[0].Status == protocol.StepRunning) &&
		assert.Equal(t, !proceeded, op.cancelled)
}

// recordingHost is a runtime and a system that only write down the calls
// that would change the host.
type recordingHost struct {
	mu    sync.Mutex
	calls []string
}

func (h *recordingHost) note(call string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.calls = append(h.calls, call)
}

func (h *recordingHost) Observe(context.Context) ([]protocol.Container, error) {
	return []protocol.Container{{Name: "geth", Image: "ethpandaops/geth:master", State: protocol.StateRunning}}, nil
}

func (h *recordingHost) LocalImage(context.Context, string) (digest, revision string, err error) {
	return "", "", nil
}

func (h *recordingHost) Pull(context.Context, string) (digest, revision string, err error) {
	return "", "", nil
}

func (h *recordingHost) Recreate(_ context.Context, name, _ string, _ time.Duration, _ bool) error {
	h.note("recreate " + name)

	return nil
}

func (h *recordingHost) RecreateAs(
	_ context.Context, name, _ string, _ time.Duration, _ bool, _ hostapi.Recreation,
) error {
	h.note("recreate " + name)

	return nil
}

func (h *recordingHost) Stop(_ context.Context, name string, _ time.Duration) error {
	h.note("stop " + name)

	return nil
}

func (h *recordingHost) Start(_ context.Context, name string) error {
	h.note("start " + name)

	return nil
}

func (h *recordingHost) Status(context.Context) (*hostapi.SystemStatus, error) {
	return &hostapi.SystemStatus{}, nil
}

func (h *recordingHost) Inventory(context.Context) (*protocol.Inventory, error) {
	return nil, errors.New("not used")
}

func (h *recordingHost) UpgradePackages(context.Context, []string) error {
	h.note("upgrade")

	return nil
}

func (h *recordingHost) Reboot(context.Context) error {
	h.note("reboot")

	return nil
}

// Nothing disruptive happens without the mark "disrupted" on disk. If the
// write that precedes the first disruptive call fails, the step fails before
// anything is touched. Otherwise a process that died after the call would be
// followed by one that reports "done, nothing disrupted", which the
// controller takes as proof that the lease cost nothing.
func TestNothingIsDisruptedWithoutTheMarkOnDisk(t *testing.T) {
	const need = "need-1"

	tests := []struct {
		name string
		run  func(o *operation, plan *operationPlan)
		plan operationPlan
		// working is what the host sees when the state can be written.
		working []string
	}{
		{
			name: "the stop of a container ahead of a reboot",
			run:  func(o *operation, plan *operationPlan) { o.drain(context.Background(), plan) },
			plan: operationPlan{reboot: []string{need}, wasRunning: []string{"geth"}}, working: []string{"stop geth"},
		},
		{
			name: "the package upgrade",
			run:  func(o *operation, plan *operationPlan) { o.applyPackages(context.Background(), plan) },
			plan: operationPlan{packageIDs: []string{need}, packages: []string{"docker-ce"}}, working: []string{"upgrade"},
		},
		{
			name: "a recreate",
			run: func(o *operation, plan *operationPlan) {
				o.recreate(context.Background(), plan, &plan.images[0])
			},
			plan: operationPlan{
				images:     []imageTask{{ids: []string{need}, container: "geth", digest: "sha256:new"}},
				wasRunning: []string{"geth"},
			},
			working: []string{"recreate geth"},
		},
	}

	for _, tt := range tests {
		for _, writable := range []bool{true, false} {
			name := tt.name + ", the state file cannot be written"
			if writable {
				name = tt.name + ", the state file can be written"
			}

			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()

				store, err := openStateStore(quietLog(), dir)
				require.NoError(t, err)

				require.NoError(t, store.Update(func(st *persistedState) {
					st.Current = &operationRecord{
						Lease:   protocol.Lease{ID: "lease-1"},
						Started: true,
						Operation: protocol.Operation{
							LeaseID: "lease-1", Phase: protocol.PhaseApplying,
							Steps: []protocol.Step{{NeedID: need, Status: protocol.StepPending}},
						},
						WasRunning: []string{"geth"},
					}
				}))

				if !writable {
					// From here on no write of the state file succeeds.
					require.NoError(t, os.RemoveAll(dir))
				}

				host := &recordingHost{}
				op := &operation{
					log: quietLog(),
					cfg: &Config{Containers: []ManagedContainer{
						{Name: "geth", Kind: protocol.KindEL, Image: "ethpandaops/geth:master"},
					}},
					runtime: host, system: host, store: store,
					now: time.Now, poll: time.Millisecond, notify: func() {},
				}

				plan := tt.plan
				tt.run(op, &plan)

				rec := store.Snapshot().Current
				require.NotNil(t, rec)

				if writable {
					assert.Equal(t, tt.working, host.calls)
					assert.True(t, rec.Operation.Disrupted)
					assert.False(t, op.failed)

					onDisk, err := openStateStore(quietLog(), dir)
					require.NoError(t, err)
					require.NotNil(t, onDisk.Snapshot().Current)
					assert.True(t, onDisk.Snapshot().Current.Operation.Disrupted, "the mark is on disk")

					return
				}

				assert.Empty(t, host.calls, "nothing was touched")
				assert.True(t, op.failed)
				assert.False(t, rec.Operation.Disrupted, "nothing was disrupted, and nothing says it was")
				assert.Equal(t, protocol.StepFailed, rec.Operation.Steps[0].Status)
				assert.Equal(t, "cannot write the state file before the first disruptive call; nothing was touched",
					rec.Operation.Steps[0].Error, "the same text every time: no temporary file name in it")
				assert.Equal(t, rec.Operation.Steps[0].Error, rec.Operation.Error)
			})
		}
	}
}
