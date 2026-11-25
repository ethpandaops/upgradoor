package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// scriptedPulls is the env's host with each Pull first run through script,
// by its count from 0; an error from script ends the pull with it.
type scriptedPulls struct {
	host.Host

	script func(ctx context.Context, n int, progress func(*protocol.PullProgress)) error

	mu    sync.Mutex
	calls int
}

func (s *scriptedPulls) Pull(
	ctx context.Context, ref string, progress func(*protocol.PullProgress),
) (string, string, error) {
	s.mu.Lock()
	n := s.calls
	s.calls++
	s.mu.Unlock()

	if err := s.script(ctx, n, progress); err != nil {
		return "", "", err
	}

	return s.Host.Pull(ctx, ref, progress)
}

func (s *scriptedPulls) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// pullEnv is an env whose geth image is pulled through script, with short
// retry pauses.
func pullEnv(
	t *testing.T, script func(ctx context.Context, n int, progress func(*protocol.PullProgress)) error,
) (*env, *scriptedPulls) {
	t.Helper()

	e := newEnv(t, agenttest.BaseScenario())
	e.host.SetRegistry(agenttest.GethImage, agenttest.GethV2, "")

	pulls := &scriptedPulls{Host: e.host, script: script}
	e.machine = pulls

	r, ok := e.start(t.Context()).(*runner)
	require.True(t, ok)

	r.pullBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

	return e, pulls
}

func TestPullRetries(t *testing.T) {
	t.Parallel()

	stalled := fmt.Errorf("pull: %w for 3m0s", host.ErrPullStalled)

	for _, tc := range []struct {
		name  string
		fail  func(n int) error
		calls int
		// deferred is the step error's tail when the lease is deferred.
		deferred string
	}{
		{
			name:  "a stall is retried and the retry resumes",
			fail:  func(n int) error { return map[bool]error{true: stalled}[n == 0] },
			calls: 2,
		},
		{
			name: "transient registry errors are retried",
			fail: func(n int) error {
				return []error{
					errors.New("read tcp 10.0.0.1:443: connection reset by peer"),
					errors.New("unexpected EOF"),
					errors.New("toomanyrequests: rate limit"),
					nil,
				}[n]
			},
			calls: 4,
		},
		{
			name:     "after three retries the lease is deferred",
			fail:     func(int) error { return errors.New("received unexpected HTTP status: 503 Service Unavailable") },
			calls:    4,
			deferred: "received unexpected HTTP status: 503 Service Unavailable, attempt 4",
		},
		{
			name:     "a permanent error is not retried",
			fail:     func(int) error { return errors.New("manifest unknown: manifest unknown") },
			calls:    1,
			deferred: "manifest unknown: manifest unknown",
		},
		{
			name: "a not found that mentions a timeout is permanent",
			fail: func(int) error {
				return errors.New("repository not found: timeout while asking")
			},
			calls:    1,
			deferred: "repository not found: timeout while asking",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, pulls := pullEnv(t, func(_ context.Context, n int, _ func(*protocol.PullProgress)) error {
				return tc.fail(n)
			})

			need := gethNeed(agenttest.GethV2)
			e.grant("lease-1", need)

			op := e.awaitFinished("lease-1")
			assert.Equal(t, tc.calls, pulls.count())
			assert.Nil(t, op.Pull, "no pull in flight after the pulling phase")

			step := stepOf(t, op, need.ID)

			if tc.deferred == "" {
				assert.Equal(t, protocol.StepDone, step.Status, step.Error)
				assert.Equal(t, []string{"pulled " + agenttest.GethImage}, e.host.PullNotes())

				return
			}

			assert.Equal(t, protocol.StepDeferred, step.Status)
			assert.Equal(t, "pull "+agenttest.GethImage+": "+tc.deferred, step.Error)
			assert.Equal(t, []string{"failed " + agenttest.GethImage}, e.host.PullNotes())
			assert.Empty(t, e.disruptive())
		})
	}
}

// The pulls end at the lease's TTL less the margin, without a retry.
func TestPullBudget(t *testing.T) {
	t.Parallel()

	e, pulls := pullEnv(t, func(ctx context.Context, _ int, _ func(*protocol.PullProgress)) error {
		<-ctx.Done()

		return fmt.Errorf("pull: %w", ctx.Err())
	})

	need := gethNeed(agenttest.GethV2)
	l := e.lease("lease-1", need)
	l.TTLSeconds, l.ExpiresAt = 1, e.now().Add(time.Second)
	require.True(t, e.follow(l))

	op := e.awaitFinished("lease-1")
	assert.Equal(t, 1, pulls.count())

	step := stepOf(t, op, need.ID)
	assert.Equal(t, protocol.StepDeferred, step.Status)
	assert.Equal(t, "pull "+agenttest.GethImage+": "+errPullBudget.Error()+"; the lease leaves 500ms for its pulls",
		step.Error)
}

func TestPullBudgetOfTheLease(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 28*time.Minute, pullBudget(&protocol.Lease{TTLSeconds: 30 * 60}))
	assert.Equal(t, 2*time.Minute, pullBudget(&protocol.Lease{TTLSeconds: 4 * 60}), "at least half the TTL")
	assert.Equal(t, pullFallbackBudget, pullBudget(&protocol.Lease{}), "no TTL known")
}

// A cancel of the lease ends the pull in flight; nothing is deferred and the
// tracker hears nothing.
func TestPullEndsWithTheLeaseCancel(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})

	e, _ := pullEnv(t, func(ctx context.Context, _ int, _ func(*protocol.PullProgress)) error {
		close(entered)
		<-ctx.Done()

		return fmt.Errorf("pull: %w", ctx.Err())
	})

	need := gethNeed(agenttest.GethV2)
	l := e.grant("lease-1", need)

	<-entered
	e.cancel(l)

	op := e.awaitFinished("lease-1")
	assert.Equal(t, protocol.StepCancelled, stepOf(t, op, need.ID).Status)
	assert.Empty(t, e.host.PullNotes())
	assert.Empty(t, e.disruptive())
}

// The pull in flight is on the operation; its counts are written at most
// every progressEvery, and the attempt as it starts.
func TestPullProgressOnTheOperation(t *testing.T) {
	t.Parallel()

	// The counts each attempt reports, one at a time.
	steps := []chan protocol.PullProgress{make(chan protocol.PullProgress), make(chan protocol.PullProgress)}
	done := make(chan struct{})

	e, _ := pullEnv(t, func(_ context.Context, n int, progress func(*protocol.PullProgress)) error {
		for p := range steps[n] {
			progress(&p)
			done <- struct{}{}
		}

		if n == 0 {
			return fmt.Errorf("pull: %w", host.ErrPullStalled)
		}

		return nil
	})

	// Long enough a budget for the clock moved on.
	need := gethNeed(agenttest.GethV2)
	l := e.lease("lease-1", need)
	l.TTLSeconds, l.ExpiresAt = 3600, e.now().Add(time.Hour)
	require.True(t, e.follow(l))

	first := e.await("attempt 1 shown", func(rec *Record) bool {
		return rec.Operation.Pull != nil && rec.Operation.Pull.Attempt == 1
	})
	assert.Equal(t, agenttest.GethImage, first.Operation.Pull.Image)
	assert.False(t, first.Operation.Pull.StartedAt.IsZero())

	steps[0] <- protocol.PullProgress{BytesDone: 10, BytesTotal: 100, LayersDone: 1, LayersTotal: 3}
	<-done

	rec := e.current().Current()
	assert.Zero(t, rec.Operation.Pull.BytesDone, "throttled")
	assert.Equal(t, first.Operation.UpdatedAt, rec.Operation.UpdatedAt)

	e.offset.Add(int64(progressEvery))
	steps[0] <- protocol.PullProgress{BytesDone: 40, BytesTotal: 100, LayersDone: 2, LayersTotal: 3}
	<-done

	rec = e.current().Current()
	pull := rec.Operation.Pull
	assert.Equal(t, protocol.PullProgress{
		Image: agenttest.GethImage, Attempt: 1, BytesDone: 40, BytesTotal: 100, LayersDone: 2, LayersTotal: 3,
		StartedAt: first.Operation.Pull.StartedAt, LastProgressAt: pull.LastProgressAt,
	}, *pull)
	assert.True(t, rec.Operation.UpdatedAt.After(first.Operation.UpdatedAt), "progress bumps the operation")

	// The retry keeps the counts until the daemon reports again.
	close(steps[0])

	second := e.await("attempt 2 shown", func(rec *Record) bool {
		return rec.Operation.Pull != nil && rec.Operation.Pull.Attempt == 2
	})
	assert.Equal(t, int64(40), second.Operation.Pull.BytesDone)
	assert.Equal(t, first.Operation.Pull.StartedAt, second.Operation.Pull.StartedAt)

	close(steps[1])

	op := e.awaitFinished("lease-1")
	assert.Nil(t, op.Pull)
	assert.Equal(t, protocol.StepDone, stepOf(t, op, need.ID).Status)
}
