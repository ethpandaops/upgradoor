package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

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
		{Name: "geth", State: containerRunning, RestartCount: r.restarts},
	}, nil
}

func (r *tickingRuntime) Start(context.Context, string) error {
	r.clock.Advance(time.Second)

	if r.clock.Now().Before(r.startsAt) {
		return errors.New("docker daemon is not up yet")
	}

	return nil
}

func (r *tickingRuntime) LocalDigest(context.Context, string) (string, error) {
	return "", errors.New("not used")
}

func (r *tickingRuntime) Pull(context.Context, string) (string, error) {
	return "", errors.New("not used")
}

func (r *tickingRuntime) Recreate(context.Context, string, string, time.Duration, bool) error {
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
