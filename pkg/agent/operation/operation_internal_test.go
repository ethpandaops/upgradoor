package operation

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// quietLog is a logger that writes nothing.
func quietLog() logrus.FieldLogger {
	l := logrus.New()
	l.SetOutput(io.Discard)

	return l
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
	return []protocol.Container{{
		Name: "geth", Image: "ethpandaops/geth:master", State: protocol.StateRunning,
		Group: "execution", Kind: protocol.KindEL,
	}}, nil
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
// miss the fault; it costs a few seconds, and it runs on every change.
func TestProceedAndCancelExcludeEachOther(t *testing.T) {
	t.Parallel()

	const (
		workers = 8
		races   = 50 // per worker
	)

	var (
		first atomic.Int64
		wg    sync.WaitGroup
	)

	for range workers {
		store, err := journal.Open(quietLog(), t.TempDir())
		require.NoError(t, err)

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
func raceProceedAndCancel(t *testing.T, store journal.Store) (proceeded, ok bool) {
	t.Helper()

	ids := []string{"image/geth/x"}

	// A fresh operation: the two writes that race after it are the ones that
	// count.
	err := store.Update(func(st *journal.State) {
		st.Current = &journal.Record{
			Lease:   protocol.Lease{ID: "lease-1"},
			Started: true,
			Operation: protocol.Operation{
				LeaseID: "lease-1", Phase: protocol.PhaseApplying,
				Steps: []protocol.Step{{NeedID: ids[0], Status: protocol.StepPending}},
			},
		}
	})
	if !assert.NoError(t, err) {
		return false, false
	}

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
		assert.NoError(t, store.Apply(func(st *journal.State) {
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

// Nothing disruptive happens without the mark "disrupted" on disk. If the
// write that precedes the first disruptive call fails, the step fails before
// anything is touched. Otherwise a process that died after the call would be
// followed by one that reports "done, nothing disrupted", which the
// controller takes as proof that the lease cost nothing.
func TestNothingIsDisruptedWithoutTheMarkOnDisk(t *testing.T) {
	t.Parallel()

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
				t.Parallel()

				dir := t.TempDir()

				store, err := journal.Open(quietLog(), dir)
				require.NoError(t, err)

				require.NoError(t, store.Update(func(st *journal.State) {
					st.Current = &journal.Record{
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
					log:     quietLog(),
					cfg:     &config.Config{},
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

					onDisk, err := journal.Open(quietLog(), dir)
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
