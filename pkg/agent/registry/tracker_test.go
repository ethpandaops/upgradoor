package registry

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

// fakeImages is a Resolver and Images; an image's revision is "rev of <digest>".
type fakeImages struct {
	mu         sync.Mutex
	registry   map[string]string
	local      map[string]string
	pulls      int
	resolves   int
	resolveErr error
	// pullAs, when set, is the digest a pull ends up with (the tag moved).
	pullAs string
	// pullErr fails the pulls.
	pullErr error
	// pullBlock, when set, holds every pull until its context ends.
	pullBlock bool
}

var _ Images = (*fakeImages)(nil)

func revisionOf(digest string) string { return "rev of " + digest }

func (h *fakeImages) Resolve(_ context.Context, ref string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.resolves++

	if h.resolveErr != nil {
		return "", h.resolveErr
	}

	return h.registry[ref], nil
}

func (h *fakeImages) LocalImage(_ context.Context, ref string) (digest, revision string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

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
	if h.pullAs != "" {
		h.local[ref] = h.pullAs
	}

	return h.local[ref], revisionOf(h.local[ref]), nil
}

func (h *fakeImages) do(fn func(h *fakeImages)) {
	h.mu.Lock()
	defer h.mu.Unlock()

	fn(h)
}

func (h *fakeImages) counts() (pulls, resolves int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.pulls, h.resolves
}

// gatedResolver holds every lookup until released; digest "" asks the fake.
type gatedResolver struct {
	*fakeImages
	digest           string
	started, release chan struct{}
}

func newGatedResolver(host *fakeImages, digest string) *gatedResolver {
	return &gatedResolver{
		fakeImages: host, digest: digest, started: make(chan struct{}), release: make(chan struct{}),
	}
}

func (g *gatedResolver) Resolve(ctx context.Context, ref string) (string, error) {
	g.started <- struct{}{}
	<-g.release

	if g.digest != "" {
		return g.digest, nil
	}

	return g.fakeImages.Resolve(ctx, ref)
}

// buildTracker is NewTracker with the concrete type.
func buildTracker(
	t *testing.T, images Images, resolver Resolver, interval time.Duration, pull bool,
	now func() time.Time,
) *tracker {
	t.Helper()

	tr, ok := NewTracker(testLogger(), images, resolver, interval, pull, now).(*tracker)
	require.True(t, ok)

	return tr
}

// checkOf is the tracker's check of one tag, from Checks.
func checkOf(tr *tracker, key string) (protocol.RegistryCheck, bool) {
	check, ok := tr.Checks()[key]

	return check, ok
}

const (
	appKey = "docker.io/library/app:latest"
	appRef = "app"
)

func newAppImages() *fakeImages {
	return &fakeImages{
		registry: map[string]string{appRef: "sha256:1"},
		local:    map[string]string{appRef: "sha256:1"},
	}
}

func TestImageWatcher(t *testing.T) {
	host := newAppImages()
	tr := buildTracker(t, host, host, time.Hour, true, time.Now)
	ctx := t.Context()

	_, ok := checkOf(tr, appKey)
	assert.False(t, ok, "nothing is known before the first check")

	tr.Track(map[string]Tag{appKey: {Ref: appRef, InUse: true}})

	due, _ := tr.nextDue()
	require.Equal(t, appKey, due, "a new tag is due at once")

	tr.checkTag(ctx, appKey)

	check, ok := checkOf(tr, appKey)
	require.True(t, ok)
	assert.Equal(t, "sha256:1", check.Digest)
	assert.True(t, check.Pulled)
	assert.Equal(t, revisionOf("sha256:1"), check.Revision, "read from the local image")
	assert.Empty(t, check.Error)

	// The controller answers: from now on it hands out the lookups.
	tr.SetFallback(false)

	due, wait := tr.nextDue()
	assert.Empty(t, due, "no lookup of its own while the controller answers")
	assert.Equal(t, maxIdle, wait)

	// The controller's lookup sees the tag moved; nothing is pulled without a window.
	host.do(func(h *fakeImages) { h.registry[appRef] = "sha256:2" })

	tr.Lookup([]string{appRef})

	due, _ = tr.nextDue()
	require.Equal(t, appKey, due)

	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr, appKey)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.False(t, check.Pulled, "no pull outside a pull window")

	pulls, resolves := host.counts()
	assert.Equal(t, 0, pulls)
	assert.Equal(t, 2, resolves)

	// The window names the image: it is pulled without another lookup.
	tr.SetWindow(&protocol.PullWindow{Images: []protocol.PullImage{{Ref: appRef, Digest: "sha256:2"}}})

	due, _ = tr.nextDue()
	require.Equal(t, appKey, due, "the window makes the pull due")

	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr, appKey)
	assert.True(t, check.Pulled)
	assert.Equal(t, revisionOf("sha256:2"), check.Revision, "read from the pulled image")

	pulls, resolves = host.counts()
	assert.Equal(t, 1, pulls)
	assert.Equal(t, 2, resolves, "the pull needs no lookup")

	due, _ = tr.nextDue()
	assert.Empty(t, due, "pulled: nothing more to do")

	// A relayed move is taken without a lookup; the local images are looked at.
	tr.SetWindow(nil)
	tr.Relay([]protocol.TagDigest{{Ref: appRef, Digest: "sha256:3", CheckedAt: time.Now()}})

	check, _ = checkOf(tr, appKey)
	assert.False(t, check.Pulled, "pulled belonged to the old digest")
	assert.Empty(t, check.Revision, "so did the revision")

	due, _ = tr.nextDue()
	require.Equal(t, appKey, due)

	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr, appKey)
	assert.Equal(t, "sha256:3", check.Digest)
	assert.False(t, check.Pulled)

	_, resolves = host.counts()
	assert.Equal(t, 2, resolves, "a relayed digest needs no lookup")

	// An older relay changes nothing.
	tr.Relay([]protocol.TagDigest{{Ref: appRef, Digest: "sha256:old", CheckedAt: time.Now().Add(-time.Hour)}})

	check, _ = checkOf(tr, appKey)
	assert.Equal(t, "sha256:3", check.Digest)

	// A roll asks for a lookup, not a pull; a relay does not answer it.
	host.do(func(h *fakeImages) { h.registry[appRef] = "sha256:3" })

	ticket := tr.Request(appRef)

	tr.Relay([]protocol.TagDigest{{Ref: appRef, Digest: "sha256:3", CheckedAt: time.Now()}})

	_, done, gone := tr.Answered(appRef, ticket)
	assert.False(t, done, "a relay is not a lookup")
	assert.False(t, gone)

	tr.checkTag(ctx, appKey)

	check, done, gone = tr.Answered(appRef, ticket)
	require.True(t, done)
	assert.False(t, gone)
	assert.Equal(t, "sha256:3", check.Digest)

	pulls, _ = host.counts()
	assert.Equal(t, 1, pulls)

	// A registry error keeps the last good digest.
	host.do(func(h *fakeImages) { h.resolveErr = errors.New("registry unreachable") })

	tr.Recheck()
	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr, appKey)
	assert.Equal(t, "sha256:3", check.Digest)
	assert.Contains(t, check.Error, "registry unreachable")

	// Checks is a copy.
	tr.Checks()[appKey] = protocol.RegistryCheck{Digest: "changed"}

	check, _ = checkOf(tr, appKey)
	assert.Equal(t, "sha256:3", check.Digest)

	// An untracked tag is forgotten.
	tr.Track(map[string]Tag{})

	_, ok = checkOf(tr, appKey)
	assert.False(t, ok)

	_, _, gone = tr.Answered(appRef, ticket)
	assert.True(t, gone)
}

// A tag no container runs is looked up on the tracker's own schedule; a tag in use is not.
func TestTrackerOwnScheduleForATagNotInUse(t *testing.T) {
	host := newAppImages()
	now := time.Unix(1_800_000_000, 0)
	tr := buildTracker(t, host, host, time.Hour, true, func() time.Time { return now })
	tr.SetFallback(false)

	tr.Track(map[string]Tag{appKey: {Ref: appRef}})
	tr.checkTag(t.Context(), appKey)

	due, wait := tr.nextDue()
	assert.Empty(t, due)
	assert.Equal(t, maxIdle, wait)

	now = now.Add(time.Hour + time.Duration(pollJitter*float64(time.Hour)))

	due, _ = tr.nextDue()
	assert.Equal(t, appKey, due, "due on its own schedule")

	tr.Track(map[string]Tag{appKey: {Ref: appRef, InUse: true}})

	due, _ = tr.nextDue()
	assert.Empty(t, due, "the controller hands out the lookups of a tag in use")
}

// An image is pulled by its tag and accepted only at the digest of the tracker's check.
func TestTrackerPullsOnlyAtTheExpectedDigest(t *testing.T) {
	host := newAppImages()
	now := time.Unix(1_800_000_000, 0)
	tr := buildTracker(t, host, host, time.Hour, true, func() time.Time { return now })
	ctx := t.Context()

	host.do(func(h *fakeImages) { h.registry[appRef] = "sha256:2" })
	tr.Track(map[string]Tag{appKey: {Ref: appRef, InUse: true}})
	tr.checkTag(ctx, appKey)

	// A window at a digest the tracker did not see: nothing is pulled.
	tr.SetWindow(&protocol.PullWindow{Images: []protocol.PullImage{{Ref: appRef, Digest: "sha256:evil"}}})

	due, _ := tr.nextDue()
	assert.Empty(t, due)

	pulls, _ := host.counts()
	assert.Equal(t, 0, pulls)

	// The tag moved before the pull: the image is refused and the tag rests.
	tr.SetWindow(&protocol.PullWindow{Images: []protocol.PullImage{{Ref: appRef, Digest: "sha256:2"}}})
	host.do(func(h *fakeImages) { h.pullAs = "sha256:9" })

	tr.checkTag(ctx, appKey)

	check, _ := checkOf(tr, appKey)
	assert.False(t, check.Pulled)
	assert.Contains(t, check.Error, "pulled digest sha256:9 differs from registry digest sha256:2")

	due, _ = tr.nextDue()
	assert.Empty(t, due, "a failed pull pauses the tag")

	// A failed pull pauses it too.
	now = now.Add(pullRetry)
	host.do(func(h *fakeImages) { h.pullErr = errors.New("no space left") })

	due, _ = tr.nextDue()
	require.Equal(t, appKey, due)

	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr, appKey)
	assert.Equal(t, "pull: no space left", check.Error)

	due, _ = tr.nextDue()
	assert.Empty(t, due)

	now = now.Add(pullRetry)
	host.do(func(h *fakeImages) { h.pullErr, h.pullAs = nil, "" })

	tr.checkTag(ctx, appKey)

	check, _ = checkOf(tr, appKey)
	assert.True(t, check.Pulled)
	assert.Equal(t, revisionOf("sha256:2"), check.Revision)

	pulls, _ = host.counts()
	assert.Equal(t, 3, pulls)
}

func TestImageWatcherRunPolls(t *testing.T) {
	host := newAppImages()
	tr := buildTracker(t, host, host, 5*time.Millisecond, true, time.Now)

	require.NoError(t, tr.Stop(), "stop before start")
	require.NoError(t, tr.Start(t.Context()))
	require.Error(t, tr.Start(t.Context()), "started once")

	tr.Track(map[string]Tag{appKey: {Ref: appRef, InUse: true}})

	require.Eventually(t, func() bool {
		_, resolves := host.counts()

		return resolves >= 4
	}, 5*time.Second, time.Millisecond, "the tracker keeps polling at its interval")

	require.NoError(t, tr.Stop())
	require.NoError(t, tr.Stop(), "stop twice")
}

// A pull is stopped when the window stops naming it at its digest, and at Stop.
func TestTrackerStopsAPullTheWindowNoLongerNames(t *testing.T) {
	host := newAppImages()
	host.do(func(h *fakeImages) {
		h.registry[appRef] = "sha256:2"
		h.pullBlock = true
	})

	window := &protocol.PullWindow{Images: []protocol.PullImage{{Ref: appRef, Digest: "sha256:2"}}}
	tr := buildTracker(t, host, host, time.Hour, true, time.Now)
	tr.Track(map[string]Tag{appKey: {Ref: appRef, InUse: true}})
	tr.SetWindow(window)

	require.NoError(t, tr.Start(t.Context()))

	for _, next := range []*protocol.PullWindow{
		nil,
		{Images: []protocol.PullImage{{Ref: appRef, Digest: "sha256:other"}}},
	} {
		require.Eventually(t, func() bool { return len(tr.Pulling()) == 1 }, 5*time.Second, time.Millisecond)
		assert.Equal(t, []string{appRef}, tr.Pulling())

		// The round's check is written after its pull ends; wait for a newer one.
		before, _ := checkOf(tr, appKey)

		tr.SetWindow(next)

		require.Eventually(t, func() bool {
			check, _ := checkOf(tr, appKey)

			return check.CheckedAt.After(before.CheckedAt)
		}, 5*time.Second, time.Millisecond)

		check, _ := checkOf(tr, appKey)
		assert.False(t, check.Pulled)
		assert.Contains(t, check.Error, "pull: ")

		// The next round: a fresh lookup clears the error, the window opens.
		tr.SetWindow(window)
		tr.Recheck()
	}

	// A dropped tag is reported while its pull runs; Stop ends the pull.
	require.Eventually(t, func() bool { return len(tr.Pulling()) == 1 }, 5*time.Second, time.Millisecond)

	tr.Track(map[string]Tag{})

	assert.Equal(t, []string{appRef}, tr.Pulling())
	require.NoError(t, tr.Stop())
	assert.Empty(t, tr.Pulling())
}

// A lookup that began before a request never answers it, even when Track re-added the tag.
func TestTrackerStaleLookupDoesNotAnswerANewRequest(t *testing.T) {
	host := newAppImages()
	res := newGatedResolver(host, "sha256:old")
	tr := buildTracker(t, host, res, time.Hour, true, time.Now)

	tr.Track(map[string]Tag{appKey: {Ref: appRef, InUse: true}})

	for range 5 {
		tr.Lookup([]string{appRef})
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		tr.checkTag(t.Context(), appKey)
	}()

	<-res.started

	tr.Track(map[string]Tag{})
	ticket := tr.Request(appRef)

	close(res.release)
	<-done

	_, answered, gone := tr.Answered(appRef, ticket)
	require.False(t, answered, "a lookup that began before the request answered it")
	require.False(t, gone)

	_, ok := checkOf(tr, appKey)
	assert.False(t, ok, "the stale result is not recorded")
}

// slowImages delays LocalImage without synchronizing, for the race detector.
type slowImages struct {
	*fakeImages
}

func (s slowImages) LocalImage(ctx context.Context, ref string) (string, string, error) {
	time.Sleep(2 * time.Millisecond)

	return s.fakeImages.LocalImage(ctx, ref)
}

// Track may run while a check looks at the local images (pinned under -race).
func TestTrackerTrackDuringACheck(t *testing.T) {
	host := newAppImages()
	tr := buildTracker(t, slowImages{host}, host, time.Hour, true, time.Now)
	tags := map[string]Tag{appKey: {Ref: appRef, InUse: true}}

	tr.Track(tags)

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			select {
			case <-stop:
				return
			default:
				tr.Track(tags)
			}
		}
	}()

	for range 5 {
		tr.checkTag(t.Context(), appKey)
	}

	close(stop)
	<-done

	check, ok := checkOf(tr, appKey)
	require.True(t, ok)
	assert.True(t, check.Pulled)
}

// The window is checked again right before a pull.
func TestTrackerNoPullAfterTheWindowDroppedTheTagDuringTheLookup(t *testing.T) {
	host := newAppImages()
	host.do(func(h *fakeImages) { h.registry[appRef] = "sha256:2" })

	res := newGatedResolver(host, "")
	tr := buildTracker(t, host, res, time.Hour, true, time.Now)

	tr.Track(map[string]Tag{appKey: {Ref: appRef, InUse: true}})
	tr.SetWindow(&protocol.PullWindow{Images: []protocol.PullImage{{Ref: appRef, Digest: "sha256:2"}}})

	done := make(chan struct{})

	go func() {
		defer close(done)

		tr.checkTag(t.Context(), appKey)
	}()

	<-res.started

	tr.SetWindow(nil)

	close(res.release)
	<-done

	pulls, _ := host.counts()
	require.Equal(t, 0, pulls, "pulled after the window dropped the tag")

	check, _ := checkOf(tr, appKey)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.False(t, check.Pulled)
}

// Request adds an untracked tag and has it looked up; Lookup ignores it.
func TestTrackerRequestAndLookupOfUntrackedTags(t *testing.T) {
	host := newAppImages()
	tr := buildTracker(t, host, host, time.Hour, true, time.Now)
	tr.SetFallback(false)

	tr.Lookup([]string{appRef})

	due, _ := tr.nextDue()
	assert.Empty(t, due)
	assert.Empty(t, tr.Checks())

	ticket := tr.Request(appRef)

	due, _ = tr.nextDue()
	require.Equal(t, appKey, due, "added and due")

	tr.checkTag(t.Context(), appKey)

	check, done, gone := tr.Answered(appRef, ticket)
	require.True(t, done)
	assert.False(t, gone)
	assert.Equal(t, "sha256:1", check.Digest)

	_, resolves := host.counts()
	assert.Equal(t, 1, resolves)
}

// Without the roll verb the tracker looks tags up and never pulls.
func TestImageWatcherDoesNotPullWhenTheHostNeverRolls(t *testing.T) {
	host := newAppImages()
	tr := buildTracker(t, host, host, time.Hour, false, time.Now)
	ctx := t.Context()

	tr.Track(map[string]Tag{appKey: {Ref: appRef, InUse: true}})
	tr.checkTag(ctx, appKey)

	check, ok := checkOf(tr, appKey)
	require.True(t, ok)
	assert.Equal(t, revisionOf("sha256:1"), check.Revision)

	host.do(func(h *fakeImages) { h.registry[appRef] = "sha256:2" })

	tr.SetWindow(&protocol.PullWindow{Images: []protocol.PullImage{{Ref: appRef, Digest: "sha256:2"}}})
	tr.Recheck()
	tr.checkTag(ctx, appKey)

	check, ok = checkOf(tr, appKey)
	require.True(t, ok)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.False(t, check.Pulled)
	assert.Empty(t, check.Revision, "the old image's revision is not the new digest's")
	assert.Empty(t, check.Error)

	due, _ := tr.nextDue()
	assert.Empty(t, due, "the window makes nothing due")

	ticket := tr.Request(appRef)
	tr.checkTag(ctx, appKey)

	check, done, _ := tr.Answered(appRef, ticket)
	require.True(t, done)
	assert.False(t, check.Pulled)

	pulls, resolves := host.counts()
	assert.Equal(t, 0, pulls)
	assert.Equal(t, 3, resolves)
	assert.Equal(t, "sha256:1", host.local[appRef], "the local tag is left where it was")
}
