package registry

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

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// fakeImages is an Images; an image's revision is "rev of <digest>".
type fakeImages struct {
	mu sync.Mutex
	// registry is what a pull of a tag yields.
	registry map[string]string
	local    map[string]string
	pulls    int
	localErr error
	// pullErr fails the pulls.
	pullErr error
	// pullBlock, when set, holds every pull until its context ends.
	pullBlock bool
	// onLocal is called by LocalImage, without the lock held.
	onLocal func()
}

var _ Images = (*fakeImages)(nil)

func revisionOf(digest string) string { return "rev of " + digest }

func (h *fakeImages) LocalImage(_ context.Context, ref string) (digest, revision string, err error) {
	h.mu.Lock()
	hook := h.onLocal
	h.mu.Unlock()

	if hook != nil {
		hook()
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.localErr != nil {
		return "", "", h.localErr
	}

	return h.local[ref], revisionOf(h.local[ref]), nil
}

func (h *fakeImages) Pull(ctx context.Context, ref string) (digest, revision string, err error) {
	h.mu.Lock()
	h.pulls++
	block := h.pullBlock
	h.mu.Unlock()

	if block {
		<-ctx.Done()

		return "", "", ctx.Err()
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.pullErr != nil {
		return "", "", h.pullErr
	}

	h.local[ref] = h.registry[ref]

	return h.local[ref], revisionOf(h.local[ref]), nil
}

func (h *fakeImages) do(fn func(h *fakeImages)) {
	h.mu.Lock()
	defer h.mu.Unlock()

	fn(h)
}

func (h *fakeImages) pulled() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.pulls
}

const (
	appKey = "docker.io/library/app:latest"
	appRef = "app"
)

// start is the time of the controller's first lookup in the tests.
var start = time.Unix(1_800_000_000, 0)

func newAppImages() *fakeImages {
	return &fakeImages{
		registry: map[string]string{appRef: "sha256:1"},
		local:    map[string]string{appRef: "sha256:1"},
	}
}

// buildTracker is NewTracker with the concrete type, following the app tag.
func buildTracker(t *testing.T, images Images, pull bool, now func() time.Time) *tracker {
	t.Helper()

	log := logrus.New()
	log.SetOutput(io.Discard)

	tr, ok := NewTracker(log, images, pull, now).(*tracker)
	require.True(t, ok)

	tr.Track(map[string]string{appKey: appRef})

	return tr
}

// lookedUp is the controller's lookup of the app tag, n minutes after start.
func lookedUp(digest string, n int) []protocol.TagDigest {
	return []protocol.TagDigest{{
		Ref: appRef, Digest: digest, CheckedAt: start.Add(time.Duration(n) * time.Minute),
	}}
}

// checkOf is the tracker's check of the app tag, from Checks.
func checkOf(tr *tracker) (protocol.RegistryCheck, bool) {
	check, ok := tr.Checks()[appKey]

	return check, ok
}

func TestTrackerFollowsTheControllersLookups(t *testing.T) {
	host := newAppImages()
	tr := buildTracker(t, host, true, time.Now)
	ctx := t.Context()

	_, ok := checkOf(tr)
	assert.False(t, ok, "nothing is known before the controller looked the tag up")
	assert.Empty(t, tr.nextDue())

	_, err := tr.Resolve(ctx, appRef)
	require.Error(t, err)

	tr.SetDigests(lookedUp("sha256:1", 0))
	require.Equal(t, appKey, tr.nextDue(), "a lookup makes a look at the local images due")

	tr.checkTag(ctx, appKey)

	check, ok := checkOf(tr)
	require.True(t, ok)
	assert.Equal(t, "sha256:1", check.Digest)
	assert.Equal(t, start, check.CheckedAt, "the controller's time")
	assert.True(t, check.Pulled)
	assert.Equal(t, revisionOf("sha256:1"), check.Revision, "read from the local image")
	assert.Empty(t, check.Error)
	assert.Empty(t, tr.nextDue())

	// The tag moved: the tracker pulls nothing itself.
	host.do(func(h *fakeImages) { h.registry[appRef] = "sha256:2" })
	tr.SetDigests(lookedUp("sha256:2", 1))

	check, _ = checkOf(tr)
	assert.False(t, check.Pulled, "pulled belonged to the old digest")
	assert.Empty(t, check.Revision, "so did the revision")

	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.False(t, check.Pulled)
	assert.Zero(t, host.pulled())
	assert.Empty(t, tr.nextDue())

	digest, err := tr.Resolve(ctx, "docker.io/library/app:latest")
	require.NoError(t, err)
	assert.Equal(t, "sha256:2", digest)

	// A lease could not pull it: the check says so for a while.
	tr.NotePull(appRef, errors.New("registry is down"))

	check, _ = checkOf(tr)
	assert.True(t, check.PullFailed)
	assert.Equal(t, "pull: registry is down", check.Error)
	assert.Empty(t, tr.nextDue())

	// A lease pulled it: the local image is looked at again.
	host.do(func(h *fakeImages) { h.local[appRef] = "sha256:2" })
	tr.NotePull(appRef, nil)
	require.Equal(t, appKey, tr.nextDue())

	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr)
	assert.True(t, check.Pulled)
	assert.False(t, check.PullFailed)
	assert.Empty(t, check.Error)
	assert.Equal(t, revisionOf("sha256:2"), check.Revision, "read from the pulled image")
	assert.Zero(t, host.pulled())
	assert.Empty(t, tr.nextDue())

	tr.NotePull("somebody/else:latest", errors.New("not followed"))

	// The same or an older lookup changes nothing and makes nothing due.
	tr.SetDigests(lookedUp("sha256:old", 0))
	tr.SetDigests(lookedUp("sha256:other", 1))

	check, _ = checkOf(tr)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.Empty(t, tr.nextDue())

	// A newer lookup of the same digest looks at the local images again: the
	// image was removed meanwhile.
	host.do(func(h *fakeImages) { h.local[appRef] = "" })
	tr.SetDigests(lookedUp("sha256:2", 2))
	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr)
	assert.False(t, check.Pulled)
	assert.Equal(t, start.Add(2*time.Minute), check.CheckedAt)

	// A failed lookup keeps the last good digest.
	failed := lookedUp("", 3)
	failed[0].Error = "registry unreachable"
	tr.SetDigests(failed)

	check, _ = checkOf(tr)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.Equal(t, "registry: registry unreachable", check.Error)

	// An error of the local images comes first.
	host.do(func(h *fakeImages) { h.localErr = errors.New("daemon down") })
	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr)
	assert.Equal(t, "local image: daemon down", check.Error)

	// Checks is a copy.
	tr.Checks()[appKey] = protocol.RegistryCheck{Digest: "changed"}

	check, _ = checkOf(tr)
	assert.Equal(t, "sha256:2", check.Digest)

	// A lookup of a tag that is not followed is ignored; an unlisted tag is forgotten.
	tr.SetDigests([]protocol.TagDigest{{Ref: "other", Digest: "sha256:x", CheckedAt: start}})
	assert.Len(t, tr.Checks(), 1)

	tr.Track(map[string]string{})

	_, ok = checkOf(tr)
	assert.False(t, ok)
}

// A tag the controller could never look up is known with its error only.
func TestTrackerATagNeverLookedUp(t *testing.T) {
	tr := buildTracker(t, newAppImages(), true, time.Now)

	failed := lookedUp("", 0)
	failed[0].Error = "404 Not Found"
	tr.SetDigests(failed)
	tr.checkTag(t.Context(), appKey)

	check, ok := checkOf(tr)
	require.True(t, ok)
	assert.Empty(t, check.Digest)
	assert.Equal(t, "registry: 404 Not Found", check.Error)

	_, err := tr.Resolve(t.Context(), appRef)
	require.Error(t, err)
}

// A failed pull of a lease counts for pullRetry, and not for another digest.
func TestTrackerAFailedPullCountsForAWhile(t *testing.T) {
	now := start
	tr := buildTracker(t, newAppImages(), true, func() time.Time { return now })

	tr.SetDigests(lookedUp("sha256:2", 0))
	tr.NotePull(appRef, errors.New("registry is down"))

	now = now.Add(pullRetry - time.Second)
	check, _ := checkOf(tr)
	assert.True(t, check.PullFailed)

	now = now.Add(time.Second)
	check, _ = checkOf(tr)
	assert.False(t, check.PullFailed)
	assert.Empty(t, check.Error)

	tr.NotePull(appRef, errors.New("registry is down"))
	tr.SetDigests(lookedUp("sha256:3", 1))

	check, _ = checkOf(tr)
	assert.False(t, check.PullFailed, "the failure was of another digest")
}

func TestTrackerALookupDuringACheckWins(t *testing.T) {
	host := newAppImages()
	tr := buildTracker(t, host, true, time.Now)

	tr.SetDigests(lookedUp("sha256:1", 0))
	host.do(func(h *fakeImages) {
		h.onLocal = func() { tr.SetDigests(lookedUp("sha256:2", 1)) }
	})

	tr.checkTag(t.Context(), appKey)

	check, _ := checkOf(tr)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.False(t, check.Pulled, "the local image is at the older digest")
	assert.Equal(t, appKey, tr.nextDue(), "the new digest is looked at next")
}

// The tag is dropped and followed again while its image is looked at: the
// result does not reach the new entry.
func TestTrackerTrackDuringACheck(t *testing.T) {
	host := newAppImages()
	tr := buildTracker(t, host, true, time.Now)

	tr.SetDigests(lookedUp("sha256:1", 0))
	host.do(func(h *fakeImages) {
		h.onLocal = func() {
			tr.Track(map[string]string{})
			tr.Track(map[string]string{appKey: appRef})
		}
	})

	tr.checkTag(t.Context(), appKey)

	_, ok := checkOf(tr)
	assert.False(t, ok)
}

func TestTrackerFallbackPulls(t *testing.T) {
	host := newAppImages()
	now := start
	tr := buildTracker(t, host, true, func() time.Time { return now })
	ctx := t.Context()

	tr.SetFallback(map[string]string{appKey: appRef}, 10*time.Minute)
	host.do(func(h *fakeImages) { h.registry[appRef] = "sha256:2" })

	// The first pull is at a random point of the interval.
	now = now.Add(10 * time.Minute)
	require.Equal(t, appKey, tr.nextDue())
	tr.checkTag(ctx, appKey)

	check, ok := checkOf(tr)
	require.True(t, ok)
	assert.Equal(t, protocol.RegistryCheck{
		Digest: "sha256:2", Revision: revisionOf("sha256:2"), Pulled: true, CheckedAt: now,
	}, check)
	assert.Equal(t, 1, host.pulled())

	digest, err := tr.Resolve(ctx, appRef)
	require.NoError(t, err)
	assert.Equal(t, "sha256:2", digest)

	now = now.Add(10*time.Minute - time.Second)
	assert.Empty(t, tr.nextDue(), "once per interval")

	// A failed pull keeps the digest and is tried again sooner.
	host.do(func(h *fakeImages) { h.pullErr = errors.New("registry is down") })

	now = now.Add(time.Second)
	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.Contains(t, check.Error, "registry is down")

	now = now.Add(pullRetry)
	assert.Equal(t, appKey, tr.nextDue())

	// The controller is back: its lookups count again, nothing is pulled.
	tr.SetFallback(nil, 0)
	assert.Empty(t, tr.nextDue())

	now = now.Add(time.Hour)
	assert.Empty(t, tr.nextDue())

	tr.SetDigests([]protocol.TagDigest{{Ref: appRef, Digest: "sha256:3", CheckedAt: now}})
	check, _ = checkOf(tr)
	assert.Equal(t, "sha256:3", check.Digest)
	assert.False(t, check.Pulled)
}

func TestTrackerFallbackNeverPullsWhenTheHostNeverRolls(t *testing.T) {
	host := newAppImages()
	now := start
	tr := buildTracker(t, host, false, func() time.Time { return now })

	tr.SetFallback(map[string]string{appKey: appRef}, time.Minute)

	now = now.Add(time.Hour)
	assert.Empty(t, tr.nextDue())
}

func TestTrackerEndingTheFallbackStopsItsPull(t *testing.T) {
	host := newAppImages()
	host.pullBlock = true

	now := start
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()

		return now
	}
	tr := buildTracker(t, host, true, clock)

	tr.SetFallback(map[string]string{appKey: appRef}, time.Minute)

	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()

	done := make(chan struct{})

	go func() {
		defer close(done)

		tr.checkTag(t.Context(), appKey)
	}()

	require.Eventually(t, func() bool { return tr.pullingNow() == appRef }, 5*time.Second, time.Millisecond)

	tr.SetFallback(nil, 0)
	<-done

	assert.Empty(t, tr.pullingNow())

	_, ok := checkOf(tr)
	assert.False(t, ok, "a stopped pull says nothing")
}
