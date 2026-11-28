package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePruner holds entry times and deletes the oldest like the store.
type fakePruner struct {
	mu      sync.Mutex
	entries []time.Time
	calls   []int
	done    chan struct{}
}

func (f *fakePruner) DeleteAuditBefore(_ context.Context, before time.Time, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	kept := f.entries[:0]
	deleted := 0

	for _, at := range f.entries {
		if at.Before(before) && deleted < limit {
			deleted++

			continue
		}

		kept = append(kept, at)
	}

	f.entries = kept
	f.calls = append(f.calls, deleted)

	if deleted < limit && f.done != nil {
		close(f.done)
		f.done = nil
	}

	return int64(deleted), nil
}

func TestAuditRetention(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	entries := make([]time.Time, 0, 2*retentionBatch+10)
	for i := range 2*retentionBatch + 5 {
		entries = append(entries, now.Add(-48*time.Hour).Add(time.Duration(i)*time.Second))
	}

	for i := range 5 {
		entries = append(entries, now.Add(-time.Duration(i)*time.Hour))
	}

	f := &fakePruner{entries: entries, done: make(chan struct{})}
	done := f.done

	r := newAuditRetention(logrus.New(), f, 24*time.Hour)
	r.now = func() time.Time { return now }
	r.pause = time.Millisecond

	require.NoError(t, r.Start(context.Background()))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no pruning")
	}

	require.NoError(t, r.Stop())

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Equal(t, []int{retentionBatch, retentionBatch, 5}, f.calls, "batch by batch")
	assert.Len(t, f.entries, 5, "the entries within the retention stay")
}

func TestAuditRetentionOff(t *testing.T) {
	f := &fakePruner{}
	r := newAuditRetention(logrus.New(), f, 0)

	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop())
	assert.Empty(t, f.calls)
}
