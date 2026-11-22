package lease

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Short verify bounds, so that an operation ends in tens of milliseconds.
const (
	testWindow  = 30 * time.Millisecond
	testTimeout = 5 * time.Second
	awaitPoll   = time.Millisecond
)

func quietLog() logrus.FieldLogger {
	l := logrus.New()
	l.SetOutput(io.Discard)

	return l
}

// testFiles is an overrides file in memory; hook runs after every Load with its count.
type testFiles struct {
	mu    sync.Mutex
	files plan.Overrides
	loads int
	hook  func(n int)
}

func (f *testFiles) Load() plan.Overrides {
	f.mu.Lock()

	out := f.files
	out.Deployed = maps.Clone(f.files.Deployed)
	out.Entries = maps.Clone(f.files.Entries)
	out.Holds = maps.Clone(f.files.Holds)
	f.loads++
	n, hook := f.loads, f.hook

	f.mu.Unlock()

	if hook != nil {
		hook(n)
	}

	return out
}

// onLoad sets the hook and restarts the count.
func (f *testFiles) onLoad(hook func(n int)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.loads, f.hook = 0, hook
}

func (f *testFiles) set(fn func(o *plan.Overrides)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(&f.files)
}

// testAudit records the lease results as "need status".
type testAudit struct {
	mu      sync.Mutex
	results []string
}

func (a *testAudit) LeaseResult(_ string, need *protocol.Need, step *protocol.Step) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.results = append(a.results, need.ID+" "+string(step.Status))
}

func (a *testAudit) got() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return slices.Clone(a.results)
}

// env is a runner on an in-memory host, a state directory and a test clock.
type env struct {
	t    *testing.T
	dir  string
	host *agenttest.Host
	// machine is the runner's host: host, unless a test wraps it.
	machine host.Host
	log     *logrus.Logger
	logs    *test.Hook
	policy  *plan.Policy
	files   *testFiles
	audit   *testAudit
	cfg     Config
	offset  atomic.Int64

	mu       sync.Mutex
	runner   Runner
	state    State
	runners  []Runner
	releases []func()
	phases   map[string][]protocol.Phase
	// ran is what ActionRan heard, by action name.
	ran map[string]string
}

func newEnv(t *testing.T, sc *agenttest.Scenario) *env {
	t.Helper()

	h, err := agenttest.NewHost(sc)
	require.NoError(t, err)

	log, logs := test.NewNullLogger()
	log.SetLevel(logrus.DebugLevel)

	e := &env{
		t: t, dir: t.TempDir(), host: h, machine: h, log: log, logs: logs,
		policy: &plan.Policy{
			Verbs: []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot},
			Packages: plan.Packages{
				Blocked: []string{"docker-ce", "containerd.io"},
			},
		},
		files:  &testFiles{files: plan.Overrides{Known: true}},
		audit:  &testAudit{},
		cfg:    Config{Window: testWindow, Timeout: testTimeout},
		phases: make(map[string][]protocol.Phase, 2),
	}

	// Release parked calls first: Stop waits for a disruptive call.
	t.Cleanup(func() {
		e.mu.Lock()
		releases, runners := e.releases, e.runners
		e.mu.Unlock()

		for _, release := range releases {
			release()
		}

		for _, r := range runners {
			assert.NoError(t, r.Stop())
		}
	})

	return e
}

func (e *env) now() time.Time { return time.Now().Add(time.Duration(e.offset.Load())) }

func (e *env) deps() *Deps {
	return &Deps{
		Host: e.machine, Policy: e.policy, Overrides: e.files, Registry: e.host, Audit: e.audit,
		Notify: e.notified, Now: e.now, ActionRan: e.actionRan,
	}
}

func (e *env) actionRan(name, inventoryHash string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ran == nil {
		e.ran = make(map[string]string, 1)
	}

	e.ran[name] = inventoryHash
}

// notified notes the phase of the current operation.
func (e *env) notified() {
	r := e.current()
	if r == nil {
		return
	}

	if op := r.Current().Reported(); op != nil {
		e.mu.Lock()
		defer e.mu.Unlock()

		seen := e.phases[op.LeaseID]
		if len(seen) == 0 || seen[len(seen)-1] != op.Phase {
			e.phases[op.LeaseID] = append(seen, op.Phase)
		}
	}
}

// phasesOf are the phases the runner entered for a lease.
func (e *env) phasesOf(id string) []protocol.Phase {
	e.mu.Lock()
	defer e.mu.Unlock()

	return slices.Clone(e.phases[id])
}

// awaitLog waits until the runner logged msg.
func (e *env) awaitLog(msg string) {
	e.t.Helper()

	require.Eventually(e.t, func() bool {
		return slices.ContainsFunc(e.logs.AllEntries(), func(l *logrus.Entry) bool { return l.Message == msg })
	}, agenttest.AwaitTimeout, awaitPoll, "never logged: %s", msg)
}

// readOnly makes the state directory unwritable until the returned func is called.
func (e *env) readOnly() (writable func()) {
	e.t.Helper()

	if os.Geteuid() == 0 {
		e.t.Skip("directory permissions do not bind root")
	}

	require.NoError(e.t, os.Chmod(e.dir, 0o500))

	writable = func() { require.NoError(e.t, os.Chmod(e.dir, 0o700)) }
	e.t.Cleanup(func() { _ = os.Chmod(e.dir, 0o700) })

	return writable
}

// addManual stores an open manual need before a runner starts.
func (e *env) addManual(need protocol.Need) {
	e.t.Helper()

	state, err := OpenState(quietLog(), e.dir)
	require.NoError(e.t, err)
	require.NoError(e.t, state.AddManual(&need))
}

// start opens the state directory and starts a runner on it in place of the current one.
func (e *env) start(ctx context.Context) Runner {
	e.t.Helper()

	state, err := OpenState(quietLog(), e.dir)
	require.NoError(e.t, err)

	r := New(e.log, &e.cfg, state, e.deps())

	e.mu.Lock()
	e.runner, e.state = r, state
	e.runners = append(e.runners, r)
	e.mu.Unlock()

	require.NoError(e.t, r.Start(ctx))

	return r
}

func (e *env) current() Runner {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.runner
}

// block parks the first call with the prefix until release.
func (e *env) block(prefix string) (entered <-chan struct{}, release func()) {
	entered, release = e.host.BlockIn(e.t, prefix)

	e.mu.Lock()
	e.releases = append(e.releases, release)
	e.mu.Unlock()

	return entered, release
}

// pull makes digest the registry's and the local image of ref.
func (e *env) pull(ref, digest string) {
	e.t.Helper()

	e.host.SetRegistry(ref, digest, "")
	_, _, err := e.host.Pull(e.t.Context(), ref)
	require.NoError(e.t, err)
}

// follow hands the runner the lease with the current operation as reported.
func (e *env) follow(l *protocol.Lease) bool {
	r := e.current()

	return r.Follow(l, r.Current().Reported())
}

func (e *env) lease(id string, needs ...protocol.Need) *protocol.Lease {
	return agenttest.Lease(e.now(), id, needs...)
}

// grant hands the runner a new lease and requires it taken.
func (e *env) grant(id string, needs ...protocol.Need) *protocol.Lease {
	e.t.Helper()

	l := e.lease(id, needs...)
	require.True(e.t, e.follow(l), "lease taken")

	return l
}

// cancel restates the lease with its cancel mark.
func (e *env) cancel(l *protocol.Lease) {
	c := *l
	c.Cancel = true
	e.follow(&c)
}

// await polls the current record until pred accepts it.
func (e *env) await(what string, pred func(rec *Record) bool) *Record {
	e.t.Helper()

	deadline := time.Now().Add(agenttest.AwaitTimeout)

	for time.Now().Before(deadline) {
		rec := e.current().Current()
		if rec != nil && pred(rec) {
			return rec
		}

		time.Sleep(awaitPoll)
	}

	require.FailNow(e.t, "timed out waiting for "+what)

	return nil
}

// awaitFinished waits until the lease's operation is terminal.
func (e *env) awaitFinished(id string) *protocol.Operation {
	e.t.Helper()

	rec := e.await(id+" finished", func(rec *Record) bool {
		return rec.Lease.ID == id && rec.Operation.Phase.Terminal()
	})

	return &rec.Operation
}

func (e *env) awaitPhase(id string, phase protocol.Phase) {
	e.t.Helper()

	e.await(id+" in phase "+string(phase), func(rec *Record) bool {
		return rec.Lease.ID == id && rec.Started && rec.Operation.Phase == phase
	})
}

// assertPhases checks the phases the runner entered for a lease, in order.
func (e *env) assertPhases(id string, want ...protocol.Phase) {
	e.t.Helper()

	// The last phase is noted right after the terminal write.
	if !assert.Eventually(e.t, func() bool { return slices.Equal(want, e.phasesOf(id)) },
		agenttest.AwaitTimeout, awaitPoll) {
		assert.Equal(e.t, want, e.phasesOf(id))
	}
}

// onDisk reads the state file as JSON.
func (e *env) onDisk() map[string]any {
	e.t.Helper()

	raw, err := os.ReadFile(filepath.Join(e.dir, StateFile))
	require.NoError(e.t, err)

	var doc map[string]any
	require.NoError(e.t, json.Unmarshal(raw, &doc))

	return doc
}

// field walks a JSON document.
func field(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()

	var cur any = doc

	for _, key := range path {
		m, ok := cur.(map[string]any)
		require.True(t, ok, "no object at %s", key)

		cur = m[key]
	}

	return cur
}

func (e *env) disruptive() []string { return agenttest.Disruptive(e.host.Calls()) }

func (e *env) assertAllRunning() {
	e.t.Helper()

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.Equal(e.t, protocol.StateRunning, e.host.Container(name).State, name)
	}
}

func stepOf(t *testing.T, op *protocol.Operation, id string) protocol.Step {
	t.Helper()

	return agenttest.StepOf(t, op, id)
}

func gethNeed(to string) protocol.Need {
	return agenttest.ImageNeedFor("geth", agenttest.GethImage, agenttest.GethV1, to)
}

func tekuNeed() protocol.Need {
	return agenttest.ImageNeedFor("teku", agenttest.TekuImage, agenttest.TekuV1, agenttest.TekuV2)
}

func vcNeed() protocol.Need {
	return agenttest.ImageNeedFor("validator", agenttest.VCImage, agenttest.VCV1, agenttest.VCV2)
}

func TestLeaseDeadline(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			got := leaseDeadline(&protocol.Lease{ExpiresAt: tt.expires, TTLSeconds: tt.ttl}, now)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStartAndStop(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	r := e.start(t.Context())

	require.Error(t, r.Start(t.Context()), "started twice")
	require.NoError(t, r.Stop())
	require.NoError(t, r.Stop(), "stopped twice")

	never := New(quietLog(), &e.cfg, func() State {
		s, err := OpenState(quietLog(), t.TempDir())
		require.NoError(t, err)

		return s
	}(), e.deps())
	require.NoError(t, never.Stop(), "stopped without a start")
}

// After a kill inside a step the next process fails that step and brings the containers back.
func TestKilledOperationIsResumedAsFailed(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)

	entered, _ := e.block("recreate geth")

	e.start(t.Context())

	needs := []protocol.Need{gethNeed(agenttest.GethV2), tekuNeed()}
	e.grant("lease-cut", needs...)

	agenttest.AwaitParked(t, entered, "recreate geth")
	assert.Equal(t, "applying", field(t, e.onDisk(), "current", "operation", "phase"))

	// The first runner stays parked in the call: as dead as it gets.
	e.start(t.Context())

	op := e.awaitFinished("lease-cut")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Contains(t, op.Error, "the agent restarted during phase applying")
	assert.Equal(t, protocol.StepFailed, stepOf(t, op, needs[0].ID).Status)
	assert.Equal(t, protocol.StepPending, stepOf(t, op, needs[1].ID).Status)

	calls := e.disruptive()
	require.GreaterOrEqual(t, len(calls), 4)
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, calls[len(calls)-3:])
	assert.NotContains(t, strings.Join(calls, "\n"), "recreate teku")
}

// An agent that died while it pulled has stopped nothing: the lease is not begun.
func TestKilledPullDefersTheLease(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")

	entered, _ := e.block("pull " + agenttest.GethImage)

	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-cut", need)

	agenttest.AwaitParked(t, entered, "pull")
	assert.Equal(t, "pulling", field(t, e.onDisk(), "current", "operation", "phase"))

	// The first runner stays parked in the call: as dead as it gets.
	e.start(t.Context())

	op := e.awaitFinished("lease-cut")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepDeferred, stepOf(t, op, need.ID).Status)
	assert.Equal(t, reasonPullInterrupted, stepOf(t, op, need.ID).Error)
	assert.Empty(t, e.disruptive())
}

// A stop carries the disruptive steps through; the next process only waits.
func TestStopFinishesTheDisruptiveSteps(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	var once sync.Once

	e.host.OnCall(func(call string) {
		if strings.HasPrefix(call, "recreate geth") {
			once.Do(sigterm)
		}
	})

	r := e.start(ctx)
	e.grant("lease-stop", gethNeed(agenttest.GethV2), tekuNeed())

	<-ctx.Done()
	require.NoError(t, r.Stop())

	assert.Equal(t, []string{
		"recreate geth " + agenttest.GethV2 + " start=true",
		"recreate teku " + agenttest.TekuV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, e.disruptive())
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest)
	assert.Equal(t, agenttest.TekuV2, e.host.Container("teku").Digest)
	assert.Equal(t, protocol.StateRunning, e.host.Container("teku").State)
	assert.Equal(t, "verifying", field(t, e.onDisk(), "current", "operation", "phase"))

	e.host.OnCall(nil)
	e.start(t.Context())

	op := e.awaitFinished("lease-stop")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

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

// A stop during the drain still issues the reboot the drain was for.
func TestStopDuringTheDrainStillReboots(t *testing.T) {
	t.Parallel()

	sc := agenttest.BaseScenario()
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

	r := e.start(ctx)
	e.grant("lease-drain", agenttest.RebootNeed())

	<-ctx.Done()
	require.NoError(t, r.Stop())

	assert.Equal(t, []string{"stop validator", "stop teku", "stop geth", "reboot"}, e.disruptive())
	assert.Equal(t, "boot-1", e.host.BootID())
	assert.Equal(t, "rebooting", field(t, e.onDisk(), "current", "operation", "phase"))

	e.host.OnCall(nil)
	e.start(t.Context())

	op := e.awaitFinished("lease-drain")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, protocol.RebootNeedID).Status)

	calls := e.disruptive()
	assert.Equal(t, []string{"start geth", "start teku", "start validator"}, calls[len(calls)-3:])
}

// A cancel outlives the process: the next process reports the step not begun as cancelled.
func TestCancelIsRememberedAcrossAnAgentRestart(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)

	entered, release := e.block("recreate geth")

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	r := e.start(ctx)
	needs := []protocol.Need{gethNeed(agenttest.GethV2), tekuNeed()}
	l := e.grant("lease-cancel", needs...)

	agenttest.AwaitParked(t, entered, "recreate geth")
	e.cancel(l)

	sigterm()
	release()
	require.NoError(t, r.Stop())

	require.Equal(t, "verifying", field(t, e.onDisk(), "current", "operation", "phase"))
	require.Equal(t, true, field(t, e.onDisk(), "current", "cancelled"))

	e.start(t.Context())

	op := e.awaitFinished("lease-cancel")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, needs[0].ID).Status)

	step := stepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepCancelled, step.Status, "also after a restart")
	assert.Contains(t, step.Error, "cancelled")
	assert.NotContains(t, strings.Join(e.disruptive(), "\n"), "recreate teku")
	assert.Equal(t, agenttest.TekuV1, e.host.Container("teku").Digest)
	e.assertAllRunning()
}

// A cancel completes the call in progress, begins nothing else and brings the host back.
func TestCancelDuringAnOperation(t *testing.T) {
	t.Parallel()

	const cancelled, done = protocol.StepCancelled, protocol.StepDone

	var (
		geth   = protocol.ImageNeedID("geth", agenttest.GethV2)
		teku   = protocol.ImageNeedID("teku", agenttest.TekuV2)
		pkgs   = agenttest.PackagesNeed().ID
		starts = []string{"start geth", "start teku", "start validator"}
	)

	tests := []struct {
		name     string
		images   bool
		gethOnly bool
		packages bool
		reboot   bool
		parkAt   string
		needs    []string
		want     []protocol.StepStatus
		geth     string
		check    func(t *testing.T, calls []string)
	}{
		{
			name: "in the first stop of the drain", images: true, reboot: true,
			parkAt: "stop validator",
			needs:  []string{geth, teku, protocol.RebootNeedID},
			want:   []protocol.StepStatus{cancelled, cancelled, cancelled},
			geth:   agenttest.GethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()
				assert.Equal(t, starts, agenttest.CallsAfter(calls, "stop validator"))
			},
		},
		{
			name: "in a recreate", images: true,
			parkAt: "recreate geth",
			needs:  []string{geth, teku},
			want:   []protocol.StepStatus{done, cancelled},
			geth:   agenttest.GethV2,
			check: func(t *testing.T, calls []string) {
				t.Helper()

				joined := strings.Join(calls, "\n")
				assert.NotContains(t, joined, "recreate teku")
				assert.NotContains(t, joined, "stop ")
				assert.NotContains(t, joined, "reboot")
			},
		},
		{
			name: "in the package upgrade, ahead of a recreate", images: true, gethOnly: true, packages: true,
			parkAt: "upgrade ",
			needs:  []string{pkgs, geth},
			want:   []protocol.StepStatus{done, cancelled},
			geth:   agenttest.GethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()
				assert.NotContains(t, strings.Join(calls, "\n"), "recreate")
			},
		},
		{
			name: "in the package upgrade, ahead of the reboot", packages: true, reboot: true,
			parkAt: "upgrade ",
			needs:  []string{pkgs, protocol.RebootNeedID},
			want:   []protocol.StepStatus{done, cancelled},
			geth:   agenttest.GethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()
				assert.NotContains(t, calls, "reboot")
				assert.Equal(t, starts, agenttest.CallsAfter(calls, "upgrade "))
			},
		},
		{
			name: "between the drain and the package upgrade", packages: true, reboot: true,
			parkAt: "stop geth",
			needs:  []string{pkgs, protocol.RebootNeedID},
			want:   []protocol.StepStatus{cancelled, cancelled},
			geth:   agenttest.GethV1,
			check: func(t *testing.T, calls []string) {
				t.Helper()
				assert.Equal(t, append([]string{"stop validator", "stop teku", "stop geth"}, starts...), calls)
			},
		},
		{
			name: "after the last step has begun", images: true, gethOnly: true,
			parkAt: "recreate geth",
			needs:  []string{geth},
			want:   []protocol.StepStatus{done},
			geth:   agenttest.GethV2,
			check:  func(*testing.T, []string) {},
		},
	}

	for _, tt := range tests {
		for _, hold := range []bool{false, true} {
			name := tt.name + ", by a cancel"
			if hold {
				name = tt.name + ", by a local hold"
			}

			t.Run(name, func(t *testing.T) {
				t.Parallel()

				sc := agenttest.BaseScenario()
				sc.RebootRequired = tt.reboot

				e := newEnv(t, sc)
				byID := map[string]protocol.Need{
					geth: gethNeed(agenttest.GethV2), teku: tekuNeed(),
					pkgs: agenttest.PackagesNeed(), protocol.RebootNeedID: agenttest.RebootNeed(),
				}

				if tt.images {
					e.pull(agenttest.GethImage, agenttest.GethV2)

					if !tt.gethOnly {
						e.pull(agenttest.TekuImage, agenttest.TekuV2)
					}
				}

				if tt.packages {
					e.host.SetUpgradable(agenttest.DockerUpgrade())
				}

				entered, release := e.block(tt.parkAt)

				e.start(t.Context())

				needs := make([]protocol.Need, 0, len(tt.needs))
				for _, id := range tt.needs {
					needs = append(needs, byID[id])
				}

				l := e.grant("lease-cancel", needs...)

				agenttest.AwaitParked(t, entered, tt.parkAt)

				if hold {
					e.files.set(func(o *plan.Overrides) {
						o.Holds = map[string]plan.Hold{protocol.HolderHost: {By: "alice", Reason: "maintenance"}}
					})
				} else {
					e.cancel(l)
				}

				release()

				op := e.awaitFinished("lease-cancel")
				assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
				assert.True(t, op.Disrupted, "the host was touched")

				for i, id := range tt.needs {
					step := stepOf(t, op, id)
					assert.Equal(t, tt.want[i], step.Status, id)

					switch {
					case tt.want[i] != cancelled:
					case hold:
						assert.Equal(t, "cancelled: host is on local hold: maintenance", step.Error, id)
					default:
						assert.Contains(t, step.Error, "cancelled", id)
					}
				}

				tt.check(t, e.disruptive())

				e.assertAllRunning()
				assert.Equal(t, tt.geth, e.host.Container("geth").Digest)
				assert.Equal(t, agenttest.TekuV1, e.host.Container("teku").Digest)
				assert.Equal(t, "boot", e.host.BootID(), "no reboot")
			})
		}
	}
}

// A local hold set while the images are pulled begins nothing.
func TestLocalHoldDuringThePull(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")

	entered, release := e.block("pull ")

	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-hold", need)

	agenttest.AwaitParked(t, entered, "pull")
	e.files.set(func(o *plan.Overrides) {
		o.Holds = map[string]plan.Hold{protocol.HolderHost: {By: "alice"}}
	})
	release()

	op := e.awaitFinished("lease-hold")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.False(t, op.Disrupted)

	step := stepOf(t, op, need.ID)
	assert.Equal(t, protocol.StepCancelled, step.Status)
	assert.Equal(t, "cancelled: host is on local hold", step.Error)
	assert.Empty(t, e.disruptive())
	assert.Equal(t, agenttest.GethV1, e.host.Container("geth").Digest)
}

// An overrides file that stops reading during a lease does not end it: only
// an explicit local hold does.
func TestUnreadableOverridesDuringALease(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")

	entered, release := e.block("pull ")

	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-typo", need)

	agenttest.AwaitParked(t, entered, "pull")
	e.files.set(func(o *plan.Overrides) { o.Known, o.Err = false, "yaml: line 3: did not find expected key" })
	release()

	op := e.awaitFinished("lease-typo")
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, need.ID).Status)
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest)
}

// After a failure a step not begun reads "not attempted", cancel and restart or not.
func TestReportAfterAFailedStepDoesNotDependOnARestart(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)
	e.host.Fail("recreate:geth", errors.New("no space left on device"))
	e.host.Fail("start:geth", errors.New("docker daemon is restarting"))

	entered, release := e.block("recreate geth")

	ctx, sigterm := context.WithCancel(t.Context())
	defer sigterm()

	r := e.start(ctx)
	needs := []protocol.Need{gethNeed(agenttest.GethV2), tekuNeed()}
	l := e.grant("lease-cancel", needs...)

	agenttest.AwaitParked(t, entered, "recreate geth")
	e.cancel(l)

	sigterm()
	release()
	require.NoError(t, r.Stop())

	require.Equal(t, "starting", field(t, e.onDisk(), "current", "operation", "phase"))
	require.Equal(t, true, field(t, e.onDisk(), "current", "cancelled"))

	e.host.Fail("start:geth", nil)
	e.start(t.Context())

	op := e.awaitFinished("lease-cancel")
	assert.NotContains(t, strings.Join(e.disruptive(), "\n"), "recreate teku")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Equal(t, protocol.StepFailed, stepOf(t, op, needs[0].ID).Status)

	step := stepOf(t, op, needs[1].ID)
	assert.Equal(t, protocol.StepPending, step.Status)
	assert.Contains(t, step.Error, "not attempted")
}

// A cancel and the beginning of a step exclude each other; run many times to hit interleavings.
func TestProceedAndCancelExcludeEachOther(t *testing.T) {
	t.Parallel()

	const (
		workers = 4
		races   = 25 // per worker
	)

	var wg sync.WaitGroup

	for range workers {
		st, err := openStore(quietLog(), t.TempDir())
		require.NoError(t, err)

		wg.Go(func() {
			for range races {
				if !raceProceedAndCancel(t, st) {
					return
				}
			}
		})
	}

	wg.Wait()
}

// raceProceedAndCancel races proceed and a cancel and reports false when they disagree.
func raceProceedAndCancel(t *testing.T, st *store) bool {
	t.Helper()

	ids := []string{"image/geth/x"}

	err := st.update(func(c *content) {
		c.Current = &Record{
			Lease: protocol.Lease{ID: "lease-1"}, Started: true,
			Operation: protocol.Operation{
				LeaseID: "lease-1", Phase: protocol.PhaseApplying,
				Steps: []protocol.Step{{NeedID: ids[0], Status: protocol.StepPending}},
			},
		}
	})
	if !assert.NoError(t, err) {
		return false
	}

	r := &runner{log: quietLog(), st: st, deps: &Deps{
		Now: time.Now, Notify: func() {}, Overrides: &testFiles{files: plan.Overrides{Known: true}},
	}}
	o := &operation{r: r, log: quietLog()}

	var (
		wg        sync.WaitGroup
		begun     bool
		proceeded bool
		start     = make(chan struct{})
	)

	wg.Go(func() {
		<-start

		proceeded = o.proceed(ids)
	})
	wg.Go(func() {
		<-start

		// What Follow writes for a cancel, and what it finds.
		assert.NoError(t, st.apply(func(c *content) {
			begun = c.Current.Operation.Steps[0].Status == protocol.StepRunning
			c.Current.Cancelled = true
		}))
	})

	close(start)
	wg.Wait()

	rec := st.current()

	return assert.True(t, rec.Cancelled) &&
		assert.Equal(t, begun, proceeded, "the cancel found the step begun: %v, proceed said: %v", begun, proceeded) &&
		assert.Equal(t, proceeded, rec.Operation.Steps[0].Status == protocol.StepRunning) &&
		assert.Equal(t, !proceeded, o.cancelled)
}

// Nothing is stopped before the disrupted mark is on disk; without it the step fails untouched.
func TestNothingIsDisruptedWithoutTheMarkOnDisk(t *testing.T) {
	t.Parallel()

	const need = "need-1"

	tests := []struct {
		name    string
		run     func(ctx context.Context, o *operation)
		lp      plan.LeasePlan
		working []string
	}{
		{
			name:    "the stop of a container ahead of a reboot",
			run:     func(ctx context.Context, o *operation) { o.drain(ctx) },
			lp:      plan.LeasePlan{Reboot: []string{need}, WasRunning: []string{"geth"}},
			working: []string{"stop geth"},
		},
		{
			name:    "the package upgrade",
			run:     func(ctx context.Context, o *operation) { o.applyPackages(ctx) },
			lp:      plan.LeasePlan{PackageIDs: []string{need}, Packages: []string{"docker-ce"}},
			working: []string{"upgrade docker-ce"},
		},
		{
			name: "a recreate",
			run:  func(ctx context.Context, o *operation) { o.recreate(ctx, &o.lp.Recreates[0]) },
			lp: plan.LeasePlan{
				Recreates:  []plan.Recreate{{IDs: []string{need}, Container: "geth", Digest: agenttest.GethV1}},
				WasRunning: []string{"geth"},
			},
			working: []string{"recreate geth " + agenttest.GethV1 + " start=true"},
		},
	}

	for _, tt := range tests {
		for _, disk := range []string{"can be written", "cannot be written", "fails once"} {
			writable := disk != "cannot be written"

			t.Run(tt.name+", the state file "+disk, func(t *testing.T) {
				t.Parallel()

				e := newEnv(t, agenttest.BaseScenario())

				st, err := openStore(quietLog(), e.dir)
				require.NoError(t, err)
				require.NoError(t, st.update(func(c *content) {
					c.Current = &Record{
						Lease: protocol.Lease{ID: "lease-1"}, Started: true,
						Operation: protocol.Operation{
							LeaseID: "lease-1", Phase: protocol.PhaseApplying,
							Steps: []protocol.Step{{NeedID: need, Status: protocol.StepPending}},
						},
						WasRunning: []string{"geth"},
					}
				}))

				if !writable {
					require.NoError(t, os.RemoveAll(e.dir))
				}

				if disk == "fails once" {
					// The first write of the mark fails; a later attempt succeeds.
					var failed atomic.Bool

					st.replace = func(path string, data []byte, perm os.FileMode) error {
						if strings.Contains(string(data), `"disrupted": true`) && failed.CompareAndSwap(false, true) {
							return errors.New("disk full")
						}

						return replaceFile(path, data, perm)
					}
				}

				r, ok := New(quietLog(), &e.cfg, st, e.deps()).(*runner)
				require.True(t, ok)

				o := &operation{r: r, log: quietLog(), lp: tt.lp}
				tt.run(t.Context(), o)

				rec := st.current()
				require.NotNil(t, rec)

				if writable {
					assert.Equal(t, tt.working, e.disruptive())
					assert.True(t, rec.Operation.Disrupted)
					assert.False(t, o.failed)

					onDisk, err := openStore(quietLog(), e.dir)
					require.NoError(t, err)
					require.NotNil(t, onDisk.current())
					assert.True(t, onDisk.current().Operation.Disrupted, "the mark is on disk")

					return
				}

				assert.Empty(t, e.disruptive(), "nothing was touched")
				assert.True(t, o.failed)
				assert.False(t, rec.Operation.Disrupted)
				assert.Equal(t, protocol.StepFailed, rec.Operation.Steps[0].Status)
				assert.Equal(t, "cannot write the state file before the first disruptive call; nothing was touched",
					rec.Operation.Steps[0].Error, "no temp file name in it")
				assert.Equal(t, rec.Operation.Steps[0].Error, rec.Operation.Error)
			})
		}
	}
}

// A lease cancelled before the start is declined at once, also while the host cannot be examined.
func TestCancelledLeaseIsNotStarted(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.host.Fail("observe", errors.New("docker daemon is restarting"))
	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	l := e.grant("lease-cancel", need)

	e.awaitLog("cannot examine the host for the lease; trying again until its deadline")
	e.cancel(l)

	op := e.awaitFinished("lease-cancel")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, stepOf(t, op, need.ID).Status)
	assert.Contains(t, stepOf(t, op, need.ID).Error, "cancelled before the operation started")
	assert.Empty(t, e.disruptive(), "nothing was touched")
}

// A lease first seen with the cancel mark is not taken.
func TestLeaseOfferedCancelledIsNotTaken(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.start(t.Context())

	l := e.lease("lease-cancel", gethNeed(agenttest.GethV2))
	l.Cancel = true

	for range 3 {
		assert.False(t, e.follow(l))
	}

	assert.Nil(t, e.current().Current())
	assert.NoFileExists(t, filepath.Join(e.dir, StateFile), "nothing was written")
	assert.Empty(t, e.disruptive())
}

// The lease and its start are on disk before anything is touched.
func TestLeaseIsPersistedBeforeActing(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)

	var (
		mu     sync.Mutex
		onDisk map[string]any
	)

	e.host.OnCall(func(call string) {
		mu.Lock()
		defer mu.Unlock()

		if onDisk == nil && strings.HasPrefix(call, "recreate ") {
			onDisk = e.onDisk()
		}
	})

	e.start(t.Context())

	l := e.lease("lease-1", gethNeed(agenttest.GethV2))
	l.ExpiresAt = e.now().Add(time.Hour)
	l.TTLSeconds = 90

	granted := e.now()
	require.True(t, e.follow(l))

	op := e.awaitFinished("lease-1")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	mu.Lock()
	defer mu.Unlock()

	require.NotNil(t, onDisk)
	assert.Equal(t, "lease-1", field(t, onDisk, "current", "lease", "id"))
	assert.Equal(t, true, field(t, onDisk, "current", "started"))
	assert.Equal(t, "applying", field(t, onDisk, "current", "operation", "phase"))

	text, ok := field(t, onDisk, "current", "deadline").(string)
	require.True(t, ok)

	deadline, err := time.Parse(time.RFC3339Nano, text)
	require.NoError(t, err)
	assert.WithinDuration(t, granted.Add(90*time.Second), deadline, 5*time.Second)
}

// A finished operation is kept while its lease is restated and forgotten once released.
func TestTerminalOperationIsDroppedWhenTheLeaseDisappears(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.start(t.Context())

	l := e.grant("lease-1", gethNeed(agenttest.GethV2))
	e.awaitFinished("lease-1")

	for range 10 {
		assert.False(t, e.follow(l))

		op := e.current().Current().Reported()
		require.NotNil(t, op)
		require.Equal(t, protocol.PhaseDone, op.Phase)
	}

	assert.NotNil(t, field(t, e.onDisk(), "current"))

	assert.False(t, e.follow(nil))
	assert.Nil(t, e.current().Current())
	assert.Nil(t, field(t, e.onDisk(), "current"))

	recreates := 0

	for _, c := range e.host.Calls() {
		if strings.HasPrefix(c, "recreate ") {
			recreates++
		}
	}

	assert.Equal(t, 1, recreates)
}

// An outcome is not forgotten before it was reported.
func TestOperationIsNotForgottenBeforeItWasReported(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)

	entered, release := e.block("recreate geth")

	r := e.start(t.Context())
	e.grant("lease-1", gethNeed(agenttest.GethV2))

	agenttest.AwaitParked(t, entered, "recreate geth")

	// The request of this answer reported the running operation.
	running := r.Current().Reported()
	require.NotNil(t, running)

	release()
	e.awaitFinished("lease-1")

	assert.False(t, r.Follow(nil, running))
	require.NotNil(t, r.Current(), "the outcome is reported before it is forgotten")

	assert.False(t, r.Follow(nil, r.Current().Reported()))
	assert.Nil(t, r.Current())
}

// A second lease waits until the open operation is finished and reported.
func TestAnotherLeaseWaitsForTheCurrentOperation(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)

	entered, release := e.block("recreate geth")

	e.start(t.Context())
	e.grant("lease-1", gethNeed(agenttest.GethV2))

	agenttest.AwaitParked(t, entered, "recreate geth")

	second := e.lease("lease-2", tekuNeed())

	for range 10 {
		assert.False(t, e.follow(second))

		rec := e.current().Current()
		require.Equal(t, "lease-1", rec.Lease.ID)
		require.Equal(t, protocol.PhaseApplying, rec.Operation.Phase)
	}

	assert.Equal(t, []string{"recreate geth " + agenttest.GethV2 + " start=true"}, e.disruptive())

	release()

	op1 := e.awaitFinished("lease-1")
	assert.Equal(t, protocol.PhaseDone, op1.Phase, op1.Error)

	assert.True(t, e.follow(second), "taken once lease-1 is reported terminal")

	op2 := e.awaitFinished("lease-2")
	assert.Equal(t, protocol.PhaseDone, op2.Phase, op2.Error)

	assert.Equal(t, []string{
		"recreate geth " + agenttest.GethV2 + " start=true",
		"start geth", "start teku", "start validator",
		"recreate teku " + agenttest.TekuV2 + " start=true",
		"start geth", "start teku", "start validator",
	}, e.disruptive())
}

// A lease withdrawn before it started is declined at once; the next lease runs.
func TestLeaseWithdrawnBeforeItStartedIsNotStarted(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)
	e.host.Fail("observe", errors.New("docker daemon is restarting"))
	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-gone", need)

	e.awaitLog("cannot examine the host for the lease; trying again until its deadline")

	// Answers to requests that reported no operation yet.
	for range 3 {
		assert.False(t, e.current().Follow(nil, nil))
	}

	op := e.awaitFinished("lease-gone")
	assert.Equal(t, protocol.PhaseDone, op.Phase)
	assert.False(t, op.Disrupted)
	assert.Equal(t, protocol.StepRefused, stepOf(t, op, need.ID).Status)
	assert.Contains(t, stepOf(t, op, need.ID).Error, "no longer offered the lease")
	assert.Empty(t, e.disruptive(), "nothing was touched")

	e.host.Fail("observe", nil)
	e.grant("lease-next", tekuNeed())

	next := e.awaitFinished("lease-next")
	assert.Equal(t, protocol.PhaseDone, next.Phase, next.Error)
	assert.Equal(t, agenttest.TekuV2, e.host.Container("teku").Digest)
	assert.Equal(t, agenttest.GethV1, e.host.Container("geth").Digest)
}

// Decisive writes take effect only on disk; a cancel and a withdrawal also in memory.
func TestDecisiveWritesReachTheDiskFirst(t *testing.T) {
	t.Parallel()

	t.Run("a new lease is not taken", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t, agenttest.BaseScenario())
		e.pull(agenttest.GethImage, agenttest.GethV2)
		e.start(t.Context())

		writable := e.readOnly()
		l := e.lease("lease-1", gethNeed(agenttest.GethV2))

		assert.False(t, e.follow(l))
		assert.Nil(t, e.current().Current())

		writable()
		assert.True(t, e.follow(l))
		assert.Equal(t, protocol.PhaseDone, e.awaitFinished("lease-1").Phase)
	})

	t.Run("a lease is not started", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t, agenttest.BaseScenario())
		skipIfRoot(t)
		e.pull(agenttest.GethImage, agenttest.GethV2)
		e.start(t.Context())

		// The disk fails once the runner looks at the lease.
		e.files.onLoad(func(n int) {
			if n == 1 {
				assert.NoError(t, os.Chmod(e.dir, 0o500))
			}
		})
		t.Cleanup(func() { _ = os.Chmod(e.dir, 0o700) })

		l := e.grant("lease-1", gethNeed(agenttest.GethV2))
		e.awaitLog("cannot persist the start of the operation; not acting")

		rec := e.current().Current()
		assert.False(t, rec.Started)
		assert.False(t, rec.Operation.Phase.Terminal())
		assert.Empty(t, e.disruptive())

		// Tried again once the disk is back.
		require.NoError(t, os.Chmod(e.dir, 0o700))
		e.follow(l)

		op := e.awaitFinished("lease-1")
		assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
		assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest)
	})

	t.Run("the rebooting phase", func(t *testing.T) {
		t.Parallel()

		sc := agenttest.BaseScenario()
		sc.RebootRequired = true

		e := newEnv(t, sc)
		entered, release := e.block("stop geth")

		e.start(t.Context())
		e.grant("lease-1", agenttest.RebootNeed())

		agenttest.AwaitParked(t, entered, "stop geth")
		e.readOnly()
		release()

		op := e.awaitFinished("lease-1")
		assert.Equal(t, protocol.PhaseFailed, op.Phase)

		step := stepOf(t, op, protocol.RebootNeedID)
		assert.Equal(t, protocol.StepFailed, step.Status)
		assert.Contains(t, step.Error, "cannot persist the state before the reboot")
		assert.NotContains(t, e.disruptive(), "reboot")
		assert.Equal(t, "boot", e.host.BootID())
		assert.True(t, e.current().Current().RebootIssuedAt.IsZero(), "the rebooting phase never took effect")
		e.assertAllRunning()
	})

	t.Run("a cancel is honoured", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t, agenttest.BaseScenario())
		e.pull(agenttest.GethImage, agenttest.GethV2)
		e.pull(agenttest.TekuImage, agenttest.TekuV2)

		entered, release := e.block("recreate geth")

		e.start(t.Context())

		teku := tekuNeed()
		l := e.grant("lease-1", gethNeed(agenttest.GethV2), teku)

		agenttest.AwaitParked(t, entered, "recreate geth")
		e.readOnly()
		e.cancel(l)
		assert.True(t, e.current().Current().Cancelled)
		release()

		op := e.awaitFinished("lease-1")
		assert.Equal(t, protocol.StepCancelled, stepOf(t, op, teku.ID).Status)
		assert.NotContains(t, strings.Join(e.disruptive(), "\n"), "recreate teku")
	})

	t.Run("a withdrawal is honoured and a decline retried", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t, agenttest.BaseScenario())
		e.pull(agenttest.GethImage, agenttest.GethV2)
		e.host.Fail("observe", errors.New("docker daemon is restarting"))
		r := e.start(t.Context())

		need := gethNeed(agenttest.GethV2)
		e.grant("lease-1", need)
		e.awaitLog("cannot examine the host for the lease; trying again until its deadline")

		writable := e.readOnly()
		assert.False(t, r.Follow(nil, nil))
		assert.True(t, r.Current().Withdrawn)

		e.awaitLog("cannot persist the refusal of the lease")
		assert.False(t, r.Current().Operation.Phase.Terminal())

		writable()
		r.Follow(nil, nil)

		op := e.awaitFinished("lease-1")
		assert.Equal(t, protocol.PhaseDone, op.Phase)
		assert.Equal(t, reasonWithdrawn, stepOf(t, op, need.ID).Error)
		assert.Empty(t, e.disruptive())
	})

	t.Run("a finished operation is not forgotten", func(t *testing.T) {
		t.Parallel()

		e := newEnv(t, agenttest.BaseScenario())
		e.pull(agenttest.GethImage, agenttest.GethV2)
		r := e.start(t.Context())

		e.grant("lease-1", gethNeed(agenttest.GethV2))
		e.awaitFinished("lease-1")

		writable := e.readOnly()
		assert.False(t, r.Follow(nil, r.Current().Reported()))
		require.NotNil(t, r.Current())

		writable()
		r.Follow(nil, r.Current().Reported())
		assert.Nil(t, r.Current())
	})
}

func skipIfRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
}

func TestFollowRows(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)

	entered, release := e.block("recreate geth")
	r := e.start(t.Context())

	// A lease without ID is not taken.
	assert.False(t, r.Follow(e.lease("", gethNeed(agenttest.GethV2)), nil))
	assert.Nil(t, r.Current())

	l := e.grant("lease-1", gethNeed(agenttest.GethV2))
	agenttest.AwaitParked(t, entered, "recreate geth")

	// A started lease the answer no longer names is finished, not withdrawn.
	assert.False(t, r.Follow(nil, nil))
	assert.False(t, r.Current().Withdrawn)

	release()

	op := e.awaitFinished("lease-1")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)

	// A terminal operation is not cancelled.
	cancelled := *l
	cancelled.Cancel = true
	assert.False(t, r.Follow(&cancelled, op))
	assert.False(t, r.Current().Cancelled)

	// Forgotten only on this lease's terminal operation.
	other := *op
	other.LeaseID = "lease-0"
	assert.False(t, r.Follow(nil, &other))
	require.NotNil(t, r.Current())

	running := *op
	running.Phase = protocol.PhaseVerifying
	assert.False(t, r.Follow(nil, &running))
	require.NotNil(t, r.Current())

	assert.False(t, r.Follow(nil, op))
	assert.Nil(t, r.Current())
}

// A lease the agent gives itself runs like any other and makes way for the
// controller's once it is finished.
func TestGrantedLeaseOfTheAgentsOwn(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.pull(agenttest.TekuImage, agenttest.TekuV2)

	r := e.start(t.Context())

	assert.False(t, r.Grant(nil), "nothing to serve")
	require.True(t, r.Grant([]protocol.Need{gethNeed(agenttest.GethV2)}))

	rec := r.Current()
	require.NotNil(t, rec)
	assert.True(t, strings.HasPrefix(rec.Lease.ID, ownLeasePrefix))
	assert.Equal(t, int64(ownLeaseTTL/time.Second), rec.Lease.TTLSeconds)

	op := e.awaitFinished(rec.Lease.ID)
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest)

	// Still silent: the finished one is replaced by the next.
	e.offset.Add(int64(time.Second))
	require.True(t, r.Grant([]protocol.Need{tekuNeed()}))

	next := r.Current().Lease.ID
	assert.NotEqual(t, rec.Lease.ID, next)

	op = e.awaitFinished(next)
	assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, agenttest.TekuV2, e.host.Container("teku").Digest)

	// The controller answers again: reported, then forgotten.
	assert.False(t, r.Follow(nil, nil))
	require.NotNil(t, r.Current())
	assert.False(t, r.Follow(nil, r.Current().Reported()))
	assert.Nil(t, r.Current())
}

func TestGrantWaitsForAnOpenLease(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.pull(agenttest.GethImage, agenttest.GethV2)
	e.host.Fail("observe", errors.New("docker daemon is restarting"))

	r := e.start(t.Context())
	e.grant("lease-open", gethNeed(agenttest.GethV2))

	assert.False(t, r.Grant([]protocol.Need{tekuNeed()}))
	assert.Equal(t, "lease-open", r.Current().Lease.ID)
}
