package agent

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
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
// What it does instead is race often enough: several stores at once for the
// whole budget, each iteration with both sides released together. With the
// mark read outside the lock about one iteration in a hundred shows it.
func TestProceedAndCancelExcludeEachOther(t *testing.T) {
	const (
		workers       = 8
		budget        = 4 * time.Second
		minIterations = 300
	)

	var (
		deadline = time.Now().Add(budget)
		total    atomic.Int64
		first    atomic.Int64
		wg       sync.WaitGroup
	)

	for range workers {
		store, err := openStateStore(quietLog(), t.TempDir())
		require.NoError(t, err)

		// The durability of the rename is not what is tested here.
		store.syncDir = func(string) error { return nil }

		wg.Add(1)

		go func() {
			defer wg.Done()

			for time.Now().Before(deadline) && !t.Failed() {
				proceeded, ok := raceProceedAndCancel(t, store)
				if !ok {
					return
				}

				total.Add(1)

				if proceeded {
					first.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	t.Logf("%d races, proceed came first in %d", total.Load(), first.Load())
	assert.GreaterOrEqual(t, total.Load(), int64(minIterations), "too few races in %s to say anything", budget)
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
	return []protocol.Container{{Name: "geth", Image: "ethpandaops/geth:master", State: containerRunning}}, nil
}

func (h *recordingHost) LocalDigest(context.Context, string) (string, error) { return "", nil }
func (h *recordingHost) Pull(context.Context, string) (string, error)        { return "", nil }

func (h *recordingHost) Recreate(_ context.Context, name, _ string, _ time.Duration, _ bool) error {
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

func (h *recordingHost) Status(context.Context) (*SystemStatus, error) { return &SystemStatus{}, nil }

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

// L4: nothing disruptive happens without the mark "disrupted" on disk. If the
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
