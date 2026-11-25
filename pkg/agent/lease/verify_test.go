package lease

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestVerifyingCatchesARestartingContainer(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.cfg = Config{Window: 80 * time.Millisecond, Timeout: 400 * time.Millisecond}
	e.pull(agenttest.GethImage, agenttest.GethV2)

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

	e.start(t.Context())

	need := gethNeed(agenttest.GethV2)
	e.grant("lease-crashloop", need)

	op := e.awaitFinished("lease-crashloop")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Contains(t, op.Error, "container geth kept restarting: ")
	assert.Contains(t, op.Error, ", never up 80ms")
	// The recreate itself worked; what failed is the operation.
	assert.Equal(t, protocol.StepDone, stepOf(t, op, need.ID).Status)

	e.assertPhases("lease-crashloop", protocol.PhasePulling, protocol.PhaseApplying, protocol.PhaseStarting,
		protocol.PhaseVerifying, protocol.PhaseFailed)
}

// steppedClock only moves when told to.
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

// tickingHost has one container, geth; every call takes one stepped second.
type tickingHost struct {
	host.Host

	clock    *steppedClock
	startsAt time.Time
	// state is geth's state; empty is running.
	state    string
	exitCode int
	// geth restarts on every listing while flapping, or before flapUntil.
	flapping  bool
	flapUntil time.Time
	restarts  int
}

func (h *tickingHost) Containers(context.Context) ([]protocol.Container, error) {
	h.clock.Advance(time.Second)

	if h.flapping || h.clock.Now().Before(h.flapUntil) {
		h.restarts++
	}

	return []protocol.Container{{
		Name: "geth", State: cmp.Or(h.state, protocol.StateRunning), ExitCode: h.exitCode,
		RestartCount: h.restarts,
	}}, nil
}

func (h *tickingHost) Start(context.Context, string) error {
	h.clock.Advance(time.Second)

	if h.clock.Now().Before(h.startsAt) {
		return errors.New("docker daemon is not up yet")
	}

	return nil
}

// verifyCfg is the verify section bringBack uses unless told otherwise.
var verifyCfg = Config{Window: 10 * time.Second, Timeout: 100 * time.Second}

// bringBack runs the starting and verifying of a disrupted geth and returns the record.
func bringBack(t *testing.T, h *tickingHost, cfg Config) *Record {
	t.Helper()

	st, err := openStore(quietLog(), t.TempDir())
	require.NoError(t, err)
	require.NoError(t, st.update(func(c *content) {
		c.Current = &Record{
			Lease: protocol.Lease{ID: "lease-1"}, Started: true,
			Operation: protocol.Operation{
				LeaseID: "lease-1", Phase: protocol.PhaseStarting, Disrupted: true,
				StartedAt: h.clock.Now(),
				Steps:     []protocol.Step{{NeedID: "image/geth/x", Status: protocol.StepDone}},
			},
			WasRunning: []string{"geth"},
			Touched:    []string{"geth"},
		}
	}))

	r, ok := New(quietLog(), &cfg, st, &Deps{
		Host: h, Policy: &plan.Policy{}, Overrides: &testFiles{files: plan.Overrides{Known: true}},
		Audit: &testAudit{}, Notify: func() {}, Now: h.clock.Now,
	}).(*runner)
	require.True(t, ok)

	r.poll = time.Millisecond
	r.work(t.Context())

	rec := st.current()
	require.NotNil(t, rec)

	return rec
}

// verify.timeout bounds starting and verifying together, on the agent's clock.
func TestStartingAndVerifyingShareOneTimeout(t *testing.T) {
	t.Parallel()

	clock := &steppedClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	began := clock.Now()

	// Starting takes 60 of the 100 seconds; geth then never comes up.
	rec := bringBack(t, &tickingHost{
		clock: clock, startsAt: began.Add(60 * time.Second), state: protocol.StateRestarting,
	}, verifyCfg)

	assert.Equal(t, protocol.PhaseFailed, rec.Operation.Phase)
	assert.Equal(t, "container geth is restarting", rec.Operation.Error)

	took := clock.Now().Sub(began)
	assert.GreaterOrEqual(t, took, 100*time.Second)
	assert.LessOrEqual(t, took, 102*time.Second, "starting and verifying took %s of 100s", took)
}

// Verifying asks each container to be up for stableUp (here the 10s window)
// at the end of the window, and waits past it for one that restarted late,
// up to maxVerify or the timeout.
func TestVerifyingOutcomes(t *testing.T) {
	t.Parallel()

	long := Config{Window: 10 * time.Second, Timeout: 10 * time.Minute}

	tests := []struct {
		name string
		host tickingHost
		// flapFor is how long geth restarts on every listing.
		flapFor time.Duration
		cfg     Config
		// err matches the error; empty for a pass.
		err string
		// took bounds how long starting and verifying took.
		tookMin, tookMax time.Duration
	}{
		{
			name: "restarts early, then stays up", flapFor: 5 * time.Second,
			cfg: verifyCfg, tookMin: 14 * time.Second, tookMax: 16 * time.Second,
		},
		{
			name: "restarts at the end of the window: waited for", flapFor: 30 * time.Second,
			cfg: verifyCfg, tookMin: 39 * time.Second, tookMax: 41 * time.Second,
		},
		{
			name: "crash loop until the timeout", host: tickingHost{state: protocol.StateRestarting, flapping: true},
			cfg: verifyCfg, err: `^container geth kept restarting: \d+ restarts, never up 10s$`,
			tookMin: 100 * time.Second, tookMax: 102 * time.Second,
		},
		{
			name: "running between restarts until maxVerify", host: tickingHost{flapping: true},
			cfg: long, err: `^container geth kept restarting: \d+ restarts, never up 10s$`,
			tookMin: maxVerify, tookMax: maxVerify + 3*time.Second,
		},
		{
			name: "exited at the end of the window", host: tickingHost{state: protocol.StateExited, exitCode: 1},
			cfg: long, err: `^container geth is exited \(exit code 1\)$`,
			tookMin: 10 * time.Second, tookMax: 13 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clock := &steppedClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
			began := clock.Now()

			h := tt.host
			h.clock, h.startsAt, h.flapUntil = clock, began, began.Add(tt.flapFor)

			rec := bringBack(t, &h, tt.cfg)

			if tt.err == "" {
				assert.Equal(t, protocol.PhaseDone, rec.Operation.Phase, rec.Operation.Error)
			} else {
				assert.Equal(t, protocol.PhaseFailed, rec.Operation.Phase)
				assert.Regexp(t, tt.err, rec.Operation.Error)
			}

			took := clock.Now().Sub(began)
			assert.GreaterOrEqual(t, took, tt.tookMin)
			assert.LessOrEqual(t, took, tt.tookMax)
		})
	}
}

// The window is measured on the agent's clock.
func TestVerifyWindowFollowsTheInjectedClock(t *testing.T) {
	t.Parallel()

	clock := &steppedClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	began := clock.Now()

	rec := bringBack(t, &tickingHost{clock: clock, startsAt: began}, verifyCfg)
	assert.Equal(t, protocol.PhaseDone, rec.Operation.Phase, rec.Operation.Error)

	took := clock.Now().Sub(began)
	assert.GreaterOrEqual(t, took, 10*time.Second, "geth was watched for the window")
	assert.LessOrEqual(t, took, 13*time.Second)
}

// Verifying follows a package upgrade and a reboot.
func TestVerifyingFollowsEveryDisruption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		need   protocol.Need
		phases []protocol.Phase
	}{
		{
			name: "packages", need: agenttest.PackagesNeed(),
			phases: []protocol.Phase{
				protocol.PhaseApplying, protocol.PhaseStarting, protocol.PhaseVerifying, protocol.PhaseDone,
			},
		},
		{
			name: "reboot", need: agenttest.RebootNeed(),
			phases: []protocol.Phase{
				protocol.PhaseDraining, protocol.PhaseRebooting, protocol.PhaseStarting,
				protocol.PhaseVerifying, protocol.PhaseDone,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sc := agenttest.BaseScenario()
			sc.RebootRequired = true
			sc.Packages.Upgradable = agenttest.DockerUpgrade()

			e := newEnv(t, sc)
			e.start(t.Context())
			e.grant("lease-1", tt.need)

			op := e.awaitFinished("lease-1")
			assert.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
			e.assertPhases("lease-1", tt.phases...)
		})
	}
}
