package operation_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestVerifyingCatchesARestartingContainer(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	e.Cfg.Verify.Window = 80 * time.Millisecond
	e.Cfg.Verify.Timeout = 400 * time.Millisecond
	e.Host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")

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

	e.Host.OnCall(func(call string) {
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
						_ = e.Host.Crash("geth")
					}
				}
			})
		})
	})

	e.Start()

	needs := agenttest.AwaitNeeds(t, e, protocol.ImageNeedID("geth", agenttest.GethV2))
	e.Ctrl.Grant(e.Lease("lease-crashloop", needs...))

	op := e.Ctrl.AwaitFinished("lease-crashloop")
	assert.Equal(t, protocol.PhaseFailed, op.Phase)
	assert.Contains(t, op.Error, "did not stay up")
	assert.Contains(t, op.Error, "geth restarted")
	// The recreate itself worked; what failed is the operation.
	assert.Equal(t, protocol.StepDone, agenttest.StepOf(t, op, needs[0].ID).Status)

	assertPhaseOrder(t, e, "lease-crashloop", protocol.PhaseFailed)
}

// verify.timeout bounds starting and verifying together, measured on the
// agent's clock.
func TestStartingAndVerifyingShareOneTimeout(t *testing.T) {
	t.Parallel()

	clock := &steppedClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	began := clock.Now()

	// Starting takes 60 of the 100 seconds; the container then never stays
	// up. Verifying has the remaining 40 seconds, not another 100.
	rt := &tickingRuntime{clock: clock, startsAt: began.Add(60 * time.Second), flapping: true}
	op, store := bringBackOperation(t, rt)

	op.Run(t.Context())

	rec := store.Snapshot().Current
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
	t.Parallel()

	clock := &steppedClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	began := clock.Now()

	op, store := bringBackOperation(t, &tickingRuntime{clock: clock, startsAt: began})

	op.Run(t.Context())

	rec := store.Snapshot().Current
	require.NotNil(t, rec)
	assert.Equal(t, protocol.PhaseDone, rec.Operation.Phase, rec.Operation.Error)

	took := clock.Now().Sub(began)
	assert.GreaterOrEqual(t, took, 10*time.Second, "the container was watched for the window")
	assert.LessOrEqual(t, took, 13*time.Second)
}
