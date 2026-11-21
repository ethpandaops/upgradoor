package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// watchedHost is a registry and image store for the watcher tests. An image
// carries the revision label "rev of <digest>".
type watchedHost struct {
	mu         sync.Mutex
	registry   map[string]string
	local      map[string]string
	pulls      int
	resolves   int
	resolveErr error
}

func revisionOf(digest string) string { return "rev of " + digest }

func (h *watchedHost) Resolve(_ context.Context, ref string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.resolves++

	if h.resolveErr != nil {
		return "", h.resolveErr
	}

	return h.registry[ref], nil
}

func (h *watchedHost) LocalImage(_ context.Context, ref string) (digest, revision string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.local[ref], revisionOf(h.local[ref]), nil
}

func (h *watchedHost) Pull(_ context.Context, ref string) (digest, revision string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.pulls++
	h.local[ref] = h.registry[ref]

	return h.local[ref], revisionOf(h.local[ref]), nil
}

func (h *watchedHost) Observe(context.Context) ([]protocol.Container, error) { return nil, nil }

func (h *watchedHost) Recreate(context.Context, string, string, time.Duration, bool) error {
	return errors.New("not used")
}

func (h *watchedHost) RecreateAs(
	context.Context, string, string, time.Duration, bool, hostapi.Recreation,
) error {
	return errors.New("not used")
}

func (h *watchedHost) Stop(context.Context, string, time.Duration) error { return nil }

func (h *watchedHost) Start(context.Context, string) error { return nil }

func (h *watchedHost) counts() (pulls, resolves int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.pulls, h.resolves
}

func TestImageWatcher(t *testing.T) {
	const key, ref = "docker.io/library/app:latest", "app"

	host := &watchedHost{
		registry: map[string]string{ref: "sha256:1"},
		local:    map[string]string{ref: "sha256:1"},
	}

	w := newImageWatcher(quietLog(), host, host, time.Hour, time.Now, true)
	ctx := t.Context()

	_, ok := w.Check(key)
	assert.False(t, ok, "nothing is known before the first check")

	w.Track(map[string]trackedTag{key: {ref: ref, running: []string{"sha256:1"}}})

	due, _ := w.nextDue()
	require.Equal(t, key, due, "a new tag is due at once")

	w.checkTag(ctx, key)

	check, ok := w.Check(key)
	require.True(t, ok)
	assert.Equal(t, "sha256:1", check.Digest)
	assert.True(t, check.Pulled)
	assert.Equal(t, revisionOf("sha256:1"), check.Revision, "read from the local image")
	assert.Empty(t, check.Error)

	due, wait := w.nextDue()
	assert.Empty(t, due, "after a check the tag rests for the poll interval")
	assert.Equal(t, maxWatchIdle, wait)

	// A container that is behind makes the watcher pull.
	host.registry[ref] = "sha256:2"

	w.RecheckAll()

	due, _ = w.nextDue()
	require.Equal(t, key, due, "recheck makes every tag due")

	w.checkTag(ctx, key)

	check, _ = w.Check(key)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.True(t, check.Pulled)
	assert.Equal(t, revisionOf("sha256:2"), check.Revision, "read from the pulled image")

	pulls, _ := host.counts()
	assert.Equal(t, 1, pulls)

	// Nobody runs the old digest any more and the local tag was moved away
	// by hand: without a container behind, the watcher leaves it alone.
	w.Track(map[string]trackedTag{key: {ref: ref, running: []string{"sha256:2"}}})

	host.local[ref] = "sha256:other"

	w.RecheckAll()
	w.checkTag(ctx, key)

	check, _ = w.Check(key)
	assert.False(t, check.Pulled)
	assert.Equal(t, revisionOf("sha256:2"), check.Revision,
		"the revision of an unchanged digest stays known")

	pulls, _ = host.counts()
	assert.Equal(t, 1, pulls, "no pull when no container is behind")

	// A manual roll asks for the image all the same.
	ticket := w.Request(key, ref)

	_, done, gone := w.Result(key, ticket)
	assert.False(t, done)
	assert.False(t, gone)

	w.checkTag(ctx, key)

	check, done, gone = w.Result(key, ticket)
	require.True(t, done)
	assert.False(t, gone)
	assert.True(t, check.Pulled)

	pulls, _ = host.counts()
	assert.Equal(t, 2, pulls)

	// A registry error keeps the last good digest.
	host.resolveErr = errors.New("registry unreachable")

	w.RecheckAll()
	w.checkTag(ctx, key)

	check, _ = w.Check(key)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.Contains(t, check.Error, "registry unreachable")

	// A tag nobody is on any more is forgotten.
	w.Track(map[string]trackedTag{})

	_, ok = w.Check(key)
	assert.False(t, ok)

	_, _, gone = w.Result(key, ticket)
	assert.True(t, gone)
}

func TestImageWatcherRunPolls(t *testing.T) {
	const key, ref = "docker.io/library/app:latest", "app"

	host := &watchedHost{
		registry: map[string]string{ref: "sha256:1"},
		local:    map[string]string{ref: "sha256:1"},
	}

	w := newImageWatcher(quietLog(), host, host, 5*time.Millisecond, time.Now, true)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		w.run(ctx)
	}()

	w.Track(map[string]trackedTag{key: {ref: ref, running: []string{"sha256:1"}}})

	require.Eventually(t, func() bool {
		_, resolves := host.counts()

		return resolves >= 4
	}, 5*time.Second, time.Millisecond, "the watcher keeps polling at its interval")

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the watcher did not stop")
	}
}

// On a host without the roll verb the watcher looks, for the report, and
// pulls nothing, whoever asks. The revision of an image that is not local is
// not known.
func TestImageWatcherDoesNotPullWhenTheHostNeverRolls(t *testing.T) {
	const key, ref = "docker.io/library/app:latest", "app"

	host := &watchedHost{
		registry: map[string]string{ref: "sha256:1"},
		local:    map[string]string{ref: "sha256:1"},
	}

	w := newImageWatcher(quietLog(), host, host, time.Hour, time.Now, false)
	ctx := t.Context()

	w.Track(map[string]trackedTag{key: {ref: ref, running: []string{"sha256:1"}}})
	w.checkTag(ctx, key)

	check, ok := w.Check(key)
	require.True(t, ok)
	assert.Equal(t, revisionOf("sha256:1"), check.Revision)

	host.registry[ref] = "sha256:2"

	w.RecheckAll()
	w.checkTag(ctx, key)

	check, ok = w.Check(key)
	require.True(t, ok)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.False(t, check.Pulled)
	assert.Empty(t, check.Revision, "the old image's revision is not the new digest's")
	assert.Empty(t, check.Error)

	ticket := w.Request(key, ref)
	w.checkTag(ctx, key)

	check, done, _ := w.Result(key, ticket)
	require.True(t, done)
	assert.False(t, check.Pulled)

	pulls, resolves := host.counts()
	assert.Equal(t, 0, pulls)
	assert.Equal(t, 3, resolves)
	assert.Equal(t, "sha256:1", host.local[ref], "the local tag is left where it was")
}
