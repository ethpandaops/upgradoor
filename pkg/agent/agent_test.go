package agent

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/lease"
	"github.com/ethpandaops/upgradoor/pkg/agent/link"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// env is one agent under test with its host, controller and configuration.
type env struct {
	t     *testing.T
	host  *agenttest.Host
	ctrl  *agenttest.Controller
	cfg   *Config
	creds *identity.Credentials
	log   *logrus.Logger
	// offset shifts the clock of the agent and the controller.
	offset atomic.Int64
}

func newEnv(t *testing.T, sc *agenttest.Scenario) *env {
	t.Helper()

	h, err := agenttest.NewHost(sc, nil)
	require.NoError(t, err)

	e := &env{t: t, host: h, log: logrus.New(), creds: agenttest.Credentials(t)}
	e.log.SetOutput(io.Discard)
	e.ctrl = agenttest.NewController(t, e.now)
	e.ctrl.Lookup = h.Resolve

	dir := t.TempDir()
	overrides := []string{protocol.OverrideImage, protocol.OverrideFlags}

	e.cfg = &Config{
		Controller: link.Config{
			URL:               e.ctrl.URL(),
			SyncInterval:      15 * time.Millisecond,
			LeaseSyncInterval: 5 * time.Millisecond,
			MinSyncInterval:   5 * time.Millisecond,
			MaxSyncInterval:   15 * time.Millisecond,
			AllowInsecure:     true,
			PublicKey:         agenttest.ControllerPublicKey(t),
		},
		StateDir: filepath.Join(dir, "state"),
		Local: local.Config{
			OverridesFile: filepath.Join(dir, "home", "overrides.yaml"),
			Socket:        local.Socket{Path: socketPath(t)},
		},
		Policy: plan.Policy{
			Verbs: []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot},
			Groups: map[string]plan.Group{
				agenttest.GroupExecution: {Overrides: overrides},
				agenttest.GroupConsensus: {Overrides: overrides},
			},
			Packages: plan.Packages{Blocked: []string{"docker-ce", "containerd.io"}},
		},
		Verify: lease.Config{Window: 30 * time.Millisecond, Timeout: 3 * time.Second},
	}
	e.cfg.ApplyDefaults()

	require.NoError(t, os.MkdirAll(filepath.Dir(e.cfg.Local.OverridesFile), 0o755))

	return e
}

// overrideEnv is an env whose geth has a command line and FixImage in the registry.
func overrideEnv(t *testing.T) *env {
	t.Helper()

	sc := agenttest.BaseScenario()
	sc.Containers[1].Command = []string{"--http", "--maxpeers=50"}

	e := newEnv(t, sc)
	e.host.SetRegistry(agenttest.FixImage, agenttest.FixV1, "rev-fix")

	return e
}

// maxSocketPath is the longest path a unix socket address holds.
const maxSocketPath = 107

// socketPath is a socket path in a short directory of its own.
func socketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp(os.TempDir(), "s")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "agent.sock")
	require.LessOrEqual(t, len(path), maxSocketPath,
		"the socket path %s is too long for a unix socket: set TMPDIR to a shorter directory", path)

	return path
}

func (e *env) now() time.Time {
	return time.Now().Add(time.Duration(e.offset.Load()))
}

func (e *env) newAgent() Service {
	e.t.Helper()

	svc, err := New(e.log, e.cfg, e.creds, &Deps{Host: e.host, Now: e.now, Version: "test"})
	require.NoError(e.t, err)

	return svc
}

// start runs an agent until the test ends or stop is called.
func (e *env) start() (stop func()) {
	e.t.Helper()

	svc := e.newAgent()
	require.NoError(e.t, svc.Start(e.t.Context()))

	var once sync.Once

	stop = func() { once.Do(func() { require.NoError(e.t, svc.Stop()) }) }
	e.t.Cleanup(stop)

	return stop
}

func (e *env) lease(id string, needs ...protocol.Need) *protocol.Lease {
	return agenttest.Lease(e.now(), id, needs...)
}

// deploy writes the generated block as Ansible does.
func (e *env) deploy(deployed map[string]plan.Deployed) {
	e.t.Helper()

	files := local.NewFiles(e.log, e.cfg.Local.OverridesFile, local.NewAudit(e.log, &local.AuditConfig{
		File: filepath.Join(e.t.TempDir(), "audit.log"), MaxSizeBytes: 1 << 20, MaxFiles: 1,
	}, e.now), e.now)
	require.NoError(e.t, files.InitBlock(deployed))
}

func (e *env) writeOverrides(content string) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(e.cfg.Local.OverridesFile, []byte(content), 0o600))
}

func (e *env) readOverrides() string {
	e.t.Helper()

	raw, err := os.ReadFile(e.cfg.Local.OverridesFile)
	require.NoError(e.t, err)

	return string(raw)
}

// stateOnDisk reads state.json as generic JSON; nil when it cannot.
func (e *env) stateOnDisk() map[string]any {
	raw, err := os.ReadFile(filepath.Join(e.cfg.StateDir, lease.StateFile))
	if err != nil {
		return nil
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}

	return out
}

// auditRecords reads the audit log, oldest first.
func (e *env) auditRecords() []local.Record {
	e.t.Helper()

	f, err := os.Open(e.cfg.Local.AuditLog.File)
	if err != nil {
		return nil
	}

	defer func() { _ = f.Close() }()

	var out []local.Record

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec local.Record
		require.NoError(e.t, json.Unmarshal(sc.Bytes(), &rec))
		out = append(out, rec)
	}

	return out
}

// auditActions are "<action> <result>" of the records.
func auditActions(records []local.Record) []string {
	out := make([]string, 0, len(records))
	for i := range records {
		out = append(out, records[i].Action+" "+records[i].Result)
	}

	return out
}

func hasNeed(req *protocol.SyncRequest, id string) bool {
	return slices.Contains(agenttest.NeedIDs(req), id)
}

func findNeed(t *testing.T, req *protocol.SyncRequest, id string) protocol.Need {
	t.Helper()

	for _, n := range req.Needs {
		if n.ID == id {
			return n
		}
	}

	require.FailNow(t, "need not reported", "%s not in %v", id, agenttest.NeedIDs(req))

	return protocol.Need{}
}

// awaitNeeds waits for the needs, pulled, and returns them in that order.
func awaitNeeds(t *testing.T, e *env, ids ...string) []protocol.Need {
	t.Helper()

	req := e.ctrl.Await("needs "+strings.Join(ids, ", "), func(req *protocol.SyncRequest) bool {
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

// awaitOverrideNeed waits for the pulled override need of a container.
func awaitOverrideNeed(t *testing.T, e *env, container string, restore bool) protocol.Need {
	t.Helper()

	var found protocol.Need

	e.ctrl.Await("an override need for "+container, func(req *protocol.SyncRequest) bool {
		for _, n := range req.Needs {
			if n.Kind == protocol.NeedOverride && n.Override.Container == container &&
				n.Override.Restore == restore {
				found = n

				return true
			}
		}

		return false
	})

	return found
}

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

func TestNewValidatesItsInput(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())

	_, err := New(e.log, e.cfg, e.creds, &Deps{})
	require.Error(t, err, "a host is required")

	_, err = New(e.log, nil, e.creds, &Deps{Host: e.host})
	require.Error(t, err)

	_, err = New(e.log, e.cfg, nil, &Deps{Host: e.host})
	require.Error(t, err)

	_, err = New(e.log, e.cfg, e.creds, nil)
	require.Error(t, err)

	raw := *e.cfg
	raw.Verify.Window = 0
	_, err = New(e.log, &raw, e.creds, &Deps{Host: e.host})
	require.ErrorContains(t, err, "without defaults")

	invalid := *e.cfg
	invalid.Policy.Verbs = []protocol.Verb{"exec"}
	_, err = New(e.log, &invalid, e.creds, &Deps{Host: e.host})
	require.ErrorContains(t, err, "unknown verb")

	svc, err := New(e.log, e.cfg, e.creds, &Deps{Host: e.host})
	require.NoError(t, err)
	require.NoError(t, svc.Stop(), "stopping an agent that never started is fine")
	require.NoError(t, svc.Start(t.Context()))
	require.Error(t, svc.Start(t.Context()), "an agent starts once")
	require.NoError(t, svc.Stop())
	require.NoError(t, svc.Stop())
	require.Error(t, svc.Start(t.Context()), "not after Stop either")
}

func TestFailedStartLeavesNothingRunning(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())

	// A file where the state directory should be.
	require.NoError(t, os.WriteFile(e.cfg.StateDir, []byte("x"), 0o600))

	svc := e.newAgent()
	require.Error(t, svc.Start(t.Context()))
	require.NoError(t, svc.Stop())
	assert.Zero(t, e.ctrl.SyncCount())
}

// Stop waits for the recreate in flight; the next agent finishes the operation.
func TestRestartInTheMiddleOfAnOperation(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.ctrl.Set(func(c *agenttest.Controller) { c.AutoRelease = false })
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")

	entered, release := e.host.BlockIn(t, "recreate geth")

	svc := e.newAgent()
	require.NoError(t, svc.Start(t.Context()))

	needs := awaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))
	e.ctrl.Grant(e.lease("lease-1", needs...))
	agenttest.AwaitParked(t, entered, "recreate geth")

	stopped := make(chan error, 1)

	go func() { stopped <- svc.Stop() }()

	select {
	case err := <-stopped:
		require.FailNow(t, "Stop returned while the recreate was in flight", "%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	require.NoError(t, <-stopped)
	assert.Equal(t, agenttest.GethV2, e.host.Container("geth").Digest, "the recreate was carried through")

	count := e.ctrl.SyncCount()
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, count, e.ctrl.SyncCount(), "a stopped agent does not sync")

	phase := field(t, e.stateOnDisk(), "current", "operation", "phase")
	assert.Contains(t, []any{"starting", "verifying"}, phase, "the waits were interrupted")

	e.start()

	op := e.ctrl.AwaitFinished("lease-1")
	require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, &op, needs[0].ID).Status)
	assert.Equal(t, 1, strings.Count(strings.Join(e.host.Calls(), "\n"), "recreate geth"))
}
