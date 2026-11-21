package operation_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/agent/operation"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// assertPhaseOrder checks the phases reported for a lease: they never go
// backwards and end with last. (Which phases a sync happens to catch
// depends on timing; the fake host is instant.)
func assertPhaseOrder(t *testing.T, e *agenttest.Env, leaseID string, last protocol.Phase) {
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

	for _, s := range e.Ctrl.AllSyncs() {
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

// quietLog is a logger that writes nothing.
func quietLog() logrus.FieldLogger {
	l := logrus.New()
	l.SetOutput(io.Discard)

	return l
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
// start and verify it, on a stepped clock: one that an agent resumes in the
// starting phase. It returns the operation and its state store.
func bringBackOperation(t *testing.T, rt *tickingRuntime) (operation.Operation, journal.Store) {
	t.Helper()

	store, err := journal.Open(quietLog(), t.TempDir())
	require.NoError(t, err)

	require.NoError(t, store.Update(func(st *journal.State) {
		st.Current = &journal.Record{
			Lease:   protocol.Lease{ID: "lease-1"},
			Started: true,
			Operation: protocol.Operation{
				LeaseID: "lease-1", Phase: protocol.PhaseStarting, Disrupted: true,
				StartedAt: rt.clock.Now(),
				Steps:     []protocol.Step{{NeedID: "image/geth/x", Status: protocol.StepDone}},
			},
			WasRunning: []string{"geth"},
			Touched:    []string{"geth"},
		}
	}))

	op := operation.New(&operation.Deps{
		Log: quietLog(),
		Config: &config.Config{
			Verify: config.Verify{Window: 10 * time.Second, Timeout: 100 * time.Second},
		},
		Runtime: rt,
		Store:   store,
		Now:     rt.clock.Now,
		Poll:    time.Millisecond,
		Notify:  func() {},
	})

	return op, store
}
