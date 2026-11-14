package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/fakehost"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// field walks nested JSON objects.
func field(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()

	var cur any = doc

	for _, key := range path {
		obj, ok := cur.(map[string]any)
		require.True(t, ok, "%v: %q is not in an object", path, key)

		cur, ok = obj[key]
		require.True(t, ok, "%v: no %q", path, key)
	}

	return cur
}

// awaitNeeds waits for a sync that reports all the given needs and returns
// them as the agent reported them, in the given order.
func awaitNeeds(t *testing.T, e *env, ids ...string) []protocol.Need {
	t.Helper()

	req := e.ctrl.await("needs "+strings.Join(ids, ", "), func(req *protocol.SyncRequest) bool {
		for _, id := range ids {
			if !hasNeed(req, id) {
				return false
			}
		}

		return true
	})

	out := make([]protocol.Need, 0, len(ids))
	for _, id := range ids {
		out = append(out, findNeed(t, &req, id))
	}

	return out
}

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
	assert.Equal(t, fakehost.StateRunning, e.host.Container("geth").State)
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
		assert.Equal(t, fakehost.StateRunning, e.host.Container(name).State, name)
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
		assert.NotEqual(t, fakehost.StateRunning, e.host.Container(name).State, name+" is drained")
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
		assert.Equal(t, fakehost.StateRunning, e.host.Container(name).State, name)
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
		assert.Equal(t, fakehost.StateRunning, e.host.Container(name).State, name)
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

	assert.Equal(t, fakehost.StateRunning, e.host.Container("teku").State)
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

	var (
		entered     = make(chan struct{})
		release     = make(chan struct{})
		enterOnce   sync.Once
		releaseOnce sync.Once
	)

	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)

	e.host.OnCall(func(call string) {
		if strings.HasPrefix(call, "recreate geth") {
			enterOnce.Do(func() {
				close(entered)
				<-release
			})
		}
	})

	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))
	e.ctrl.grant(e.lease("lease-1", needs...))

	select {
	case <-entered:
	case <-time.After(awaitTimeout):
		require.FailNow(t, "the operation never started")
	}

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
	var (
		entered = make(chan struct{})
		release = make(chan struct{})
		once    sync.Once
	)

	e.host.OnCall(func(call string) {
		if strings.HasPrefix(call, "recreate geth") {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	})

	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-1", needs[0]))

	select {
	case <-entered:
	case <-time.After(awaitTimeout):
		require.FailNow(t, "the first operation never started")
	}

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

	close(release)

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

	var (
		parked  atomic.Bool
		inCall  = make(chan struct{})
		release = make(chan struct{})
	)

	e.host.OnCall(func(call string) {
		if strings.HasPrefix(call, "recreate geth") && parked.CompareAndSwap(false, true) {
			close(inCall)
			<-release
		}
	})

	dead := e.newAgent()
	require.NoError(t, dead.Start(ctx))

	t.Cleanup(func() {
		close(release)
		require.NoError(t, dead.Stop())
	})

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2), protocol.ImageNeedID("teku", tekuV2))
	e.ctrl.grant(e.lease("lease-cut", needs...))

	<-inCall
	kill()

	assert.Equal(t, "applying", field(t, e.stateOnDisk(), "current", "operation", "phase"))

	// Nobody knows what the cut-off step left behind. The next agent
	// process does not guess: it fails the step, brings the containers
	// back and reports.
	e.host.OnCall(nil)
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
	assert.Equal(t, fakehost.StateRunning, e.host.Container("teku").State)
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

// awaitLeaseOnDisk waits until the agent has persisted a lease.
func awaitLeaseOnDisk(t *testing.T, e *env, id string) {
	t.Helper()

	require.Eventually(t, func() bool {
		cur, ok := e.stateOnDisk()["current"].(map[string]any)
		if !ok {
			return false
		}

		lease, ok := cur["lease"].(map[string]any)

		return ok && lease["id"] == id
	}, awaitTimeout, 2*time.Millisecond)
}

// A Docker daemon that does not answer when the lease arrives is no reason
// to report a failed operation, which would halt the rollout: the agent
// looks again for as long as the lease is good.
func TestHostThatCannotBeExaminedDoesNotFailTheLease(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.host.SetRegistry(gethImage, gethV2, "")
	e.start()

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", gethV2))

	e.host.Fail("observe", errors.New("docker daemon is restarting"))
	e.ctrl.grant(e.lease("lease-retry", needs...))

	awaitLeaseOnDisk(t, e, "lease-retry")
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

	e.host.Fail("observe", errors.New("docker daemon is gone"))
	e.ctrl.grant(e.lease("lease-blind", needs...))

	// Let the agent look (in vain) while the lease is still good, then let
	// the lease run out.
	awaitLeaseOnDisk(t, e, "lease-blind")
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
