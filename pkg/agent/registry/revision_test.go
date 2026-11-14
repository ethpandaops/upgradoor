package registry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
)

// TestRevisionFailuresNeverFailResolve walks every way the revision lookup can
// go wrong. The digest must come back regardless, and the failed lookup must
// not be repeated on the next resolution.
func TestRevisionFailuresNeverFailResolve(t *testing.T) {
	unknown := "sha256:" + strings.Repeat("7", 64)

	manifestWithConfig := func(t *testing.T, digest string) []byte {
		t.Helper()

		return mustJSON(t, map[string]any{"config": fakeDescriptor{Digest: digest}})
	}

	indexOf := func(t *testing.T, digest string) []byte {
		t.Helper()

		return indexBody(t, fakeDescriptor{Digest: digest, Platform: &fakePlatform{OS: "linux", Architecture: "amd64"}})
	}

	cases := []struct {
		name string
		// build returns the body the tag points at.
		build func(t *testing.T, f *fakeRegistry) []byte
		// gets is the number of GETs the one lookup costs.
		gets int
	}{
		{"manifest is not json", func(_ *testing.T, _ *fakeRegistry) []byte { return []byte("<html>") }, 1},
		{"manifest without config", func(t *testing.T, _ *fakeRegistry) []byte {
			t.Helper()

			return mustJSON(t, map[string]any{"layers": []string{}})
		}, 1},
		{"config digest is not a digest", func(t *testing.T, _ *fakeRegistry) []byte {
			t.Helper()

			return manifestWithConfig(t, "sha256:../../../token")
		}, 1},
		{"config blob missing", func(t *testing.T, _ *fakeRegistry) []byte {
			t.Helper()

			return manifestWithConfig(t, unknown)
		}, 2},
		{"config is not json", func(t *testing.T, f *fakeRegistry) []byte {
			t.Helper()

			return manifestWithConfig(t, f.add([]byte("not json")))
		}, 2},
		{"config content does not match its digest", func(t *testing.T, f *fakeRegistry) []byte {
			t.Helper()

			f.handle(kindBlob+unknown, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(configBody(t, "forged"))
			})

			return manifestWithConfig(t, unknown)
		}, 2},
		{"config too large", func(t *testing.T, f *fakeRegistry) []byte {
			t.Helper()

			f.handle(kindBlob+unknown, func(w http.ResponseWriter, _ *http.Request) {
				chunk := []byte(strings.Repeat(" ", 1<<20))
				for range 5 {
					_, _ = w.Write(chunk)
				}
			})

			return manifestWithConfig(t, unknown)
		}, 2},
		{"config body cut short", func(t *testing.T, f *fakeRegistry) []byte {
			t.Helper()

			f.handle(kindBlob+unknown, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "{")
			})

			return manifestWithConfig(t, unknown)
		}, 2},
		{"index with only attestations", func(t *testing.T, _ *fakeRegistry) []byte {
			t.Helper()

			return indexBody(t, fakeDescriptor{
				Digest:   unknown,
				Platform: &fakePlatform{OS: platformUnknown, Architecture: platformUnknown},
			})
		}, 1},
		{"platform manifest missing", func(t *testing.T, _ *fakeRegistry) []byte {
			t.Helper()

			return indexOf(t, unknown)
		}, 2},
		{"platform manifest digest is not a digest", func(t *testing.T, _ *fakeRegistry) []byte {
			t.Helper()

			return indexOf(t, "latest")
		}, 1},
		{"platform manifest is not json", func(t *testing.T, f *fakeRegistry) []byte {
			t.Helper()

			return indexOf(t, f.add([]byte("[")))
		}, 2},
		{"platform manifest is an index again", func(t *testing.T, f *fakeRegistry) []byte {
			t.Helper()

			return indexOf(t, f.indexDigest)
		}, 2},
		{"platform manifest server error", func(t *testing.T, f *fakeRegistry) []byte {
			t.Helper()

			f.handle(kindManifst+unknown, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			})

			return indexOf(t, unknown)
		}, 2},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRegistry(t, false)
			want := f.tag("t", c.build(t, f))

			log, hook := logrustest.NewNullLogger()
			log.SetLevel(logrus.DebugLevel)

			r, err := New(log, Options{PlainHTTP: []string{f.host()}, Timeout: 5 * time.Second, Platform: plainAMD64})
			require.NoError(t, err)
			t.Cleanup(r.Close)

			for i := 1; i <= 3; i++ {
				digest, revision, err := r.Resolve(t.Context(), f.ref("t"))
				require.NoError(t, err)
				require.Equal(t, want, digest)
				require.Empty(t, revision)
				require.Equal(t, i, f.heads())
				require.Equal(t, c.gets, f.gets(), "the lookup runs once per digest, failed or not")
			}

			require.Len(t, hook.AllEntries(), 1, "one debug line for the one attempt")
			require.Equal(t, logrus.DebugLevel, hook.LastEntry().Level)
			require.Equal(t, want, hook.LastEntry().Data["digest"])
		})
	}
}

func TestRevisionManifestByDigestFailures(t *testing.T) {
	// The tag answers the HEAD, but the manifest behind its digest cannot be
	// fetched or is not what its digest says.
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"server error": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"forged":       func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"config":{}}`) },
	}

	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeRegistry(t, false)
			f.handle(kindManifst+f.indexDigest, handler)

			r := newResolver(t, f, nil)

			for range 2 {
				digest, revision, err := r.Resolve(t.Context(), f.ref(tagMulti))
				require.NoError(t, err)
				require.Equal(t, f.indexDigest, digest)
				require.Empty(t, revision)
			}

			require.Equal(t, 1, f.gets())
		})
	}
}

func TestRevisionLabelAbsent(t *testing.T) {
	f := newFakeRegistry(t, false)
	cfg := f.add(configBody(t, ""))
	want := f.tag("nolabel", mustJSON(t, map[string]any{"config": fakeDescriptor{Digest: cfg}}))

	// A config without any labels object at all.
	bare := f.add([]byte(`{"architecture":"amd64"}`))
	wantBare := f.tag("bare", mustJSON(t, map[string]any{"config": fakeDescriptor{Digest: bare}}))

	r := newResolver(t, f, nil)

	for range 2 {
		digest, revision, err := r.Resolve(t.Context(), f.ref("nolabel"))
		require.NoError(t, err)
		require.Equal(t, want, digest)
		require.Empty(t, revision)
	}

	require.Equal(t, 2, f.gets())

	digest, revision, err := r.Resolve(t.Context(), f.ref("bare"))
	require.NoError(t, err)
	require.Equal(t, wantBare, digest)
	require.Empty(t, revision)
}

func TestRevisionIsCachedPerRepository(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	// Two tags on the same digest share one lookup.
	f.set(func(f *fakeRegistry) { f.tags["other"] = f.indexDigest })

	for _, tag := range []string{tagMulti, "other", tagMulti} {
		_, revision, err := r.Resolve(t.Context(), f.ref(tag))
		require.NoError(t, err)
		require.Equal(t, revAMD64, revision)
	}

	require.Equal(t, 3, f.gets())
}

func TestConcurrentResolveLooksUpOnce(t *testing.T) {
	f := newFakeRegistry(t, false)
	release := make(chan struct{})

	body := configBody(t, revAMD64)
	f.handle(kindBlob+f.amd64Config, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}

		_, _ = w.Write(body)
	})

	r := newResolver(t, f, nil)

	const workers = 8

	var wg sync.WaitGroup

	revisions := make([]string, workers)
	errs := make([]error, workers)

	for i := range workers {
		wg.Go(func() {
			_, revisions[i], errs[i] = r.Resolve(t.Context(), f.ref(tagMulti))
		})
	}

	// Every worker gets past its HEAD; one of them owns the lookup and waits
	// in the blob handler, the others wait for it.
	require.Eventually(t, func() bool {
		return f.heads() == workers && f.count(http.MethodGet, kindBlob) == 1
	}, 5*time.Second, 5*time.Millisecond)

	close(release)
	wg.Wait()

	for i := range workers {
		require.NoError(t, errs[i])
		require.Equal(t, revAMD64, revisions[i])
	}

	require.Equal(t, workers, f.heads())
	require.Equal(t, 3, f.gets())
}

func TestRevisionWaiterGivesUpWithItsContext(t *testing.T) {
	r, err := New(testLogger(), Options{})
	require.NoError(t, err)

	ref, err := ParseReference("ghcr.io/org/app:t")
	require.NoError(t, err)

	digest := "sha256:" + strings.Repeat("1", 64)

	// Somebody else owns the lookup and never finishes.
	entry, owner := r.claimRevision("ghcr.io/org/app@" + digest)
	require.True(t, owner)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.Empty(t, r.revision(ctx, ref, digest, nil))

	// Once the owner is done, waiters see its result.
	entry.revision = "abc"
	close(entry.ready)

	require.Equal(t, "abc", r.revision(t.Context(), ref, digest, nil))
}

func TestRevisionCacheIsBounded(t *testing.T) {
	r, err := New(testLogger(), Options{})
	require.NoError(t, err)

	for i := range maxRevisions + 10 {
		_, owner := r.claimRevision(fmt.Sprintf("k%d", i))
		require.True(t, owner)
	}

	require.Len(t, r.revisions, maxRevisions)
	require.Len(t, r.revisionOrder, maxRevisions)

	_, owner := r.claimRevision(fmt.Sprintf("k%d", maxRevisions+9))
	require.False(t, owner, "recent entries stay")

	_, owner = r.claimRevision("k0")
	require.True(t, owner, "the oldest entries were dropped")
}

func TestPlatformPick(t *testing.T) {
	var index manifest

	_, err := platform{os: "linux", arch: "amd64"}.pick(&index)
	require.Error(t, err)
}

// fakeClock is a settable time source for Resolver.now.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// TestInterruptedRevisionLookupIsNotCached: a lookup that dies with the
// caller's context must not leave the digest without a revision for good.
func TestInterruptedRevisionLookupIsNotCached(t *testing.T) {
	for _, kind := range []string{"cancelled", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeRegistry(t, false)
			r := newResolver(t, f, nil)

			ctx, cancel := context.WithCancel(t.Context())
			if kind == "deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), time.Hour)
			}

			defer cancel()

			// The first child-manifest GET outlives its caller's context.
			var once sync.Once

			f.handle(kindManifst+f.amd64Digest, func(w http.ResponseWriter, req *http.Request) {
				interrupted := false

				once.Do(func() {
					interrupted = true

					cancel()
					<-req.Context().Done()
				})

				if !interrupted {
					f.serveContent(w, req, f.amd64Digest, "")
				}
			})

			digest, revision, err := r.Resolve(ctx, f.ref(tagMulti))
			require.NoError(t, err, "the digest was already known")
			require.Equal(t, f.indexDigest, digest)
			require.Empty(t, revision)

			require.Empty(t, r.revisions, "nothing is cached for an interrupted lookup")
			require.Empty(t, r.revisionOrder)

			// A fresh context starts the lookup over, at once.
			_, revision, err = r.Resolve(t.Context(), f.ref(tagMulti))
			require.NoError(t, err)
			require.Equal(t, revAMD64, revision)

			gets := f.gets()

			_, revision, err = r.Resolve(t.Context(), f.ref(tagMulti))
			require.NoError(t, err)
			require.Equal(t, revAMD64, revision)
			require.Equal(t, gets, f.gets(), "and the result is cached")
			require.Len(t, r.revisionOrder, 1)
		})
	}
}

// TestFailedRevisionLookupIsRetriedLater: a transient failure costs nothing
// on the following polls, is tried again after the retry interval, and the
// success is then cached.
func TestFailedRevisionLookupIsRetriedLater(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	r.now = clock.Now

	f.handle(kindBlob+f.amd64Config, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	resolve := func(want string) {
		t.Helper()

		digest, revision, err := r.Resolve(t.Context(), f.ref(tagMulti))
		require.NoError(t, err)
		require.Equal(t, f.indexDigest, digest)
		require.Equal(t, want, revision)
	}

	resolve("")
	require.Equal(t, 3, f.gets(), "index, platform manifest, refused config")

	// Polls inside the retry interval stay at one HEAD each.
	for range 5 {
		resolve("")
	}

	clock.Advance(revisionRetryInterval - time.Second)
	resolve("")
	require.Equal(t, 3, f.gets())
	require.Equal(t, 7, f.heads())

	// The registry recovers; the next poll after the interval finds out.
	f.set(func(f *fakeRegistry) { delete(f.override, kindBlob+f.amd64Config) })
	clock.Advance(time.Second)
	resolve(revAMD64)
	require.Equal(t, 6, f.gets())

	clock.Advance(100 * revisionRetryInterval)
	resolve(revAMD64)
	require.Equal(t, 6, f.gets(), "a success is cached for good")
	require.Len(t, r.revisionOrder, 1, "a retry reuses the cache slot")
}

// TestFailingRevisionLookupGivesUp: a lookup that keeps failing is bounded.
func TestFailingRevisionLookupGivesUp(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	r.now = clock.Now

	f.handle(kindBlob+f.amd64Config, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	for range maxRevisionAttempts + 5 {
		_, revision, err := r.Resolve(t.Context(), f.ref(tagMulti))
		require.NoError(t, err)
		require.Empty(t, revision)

		clock.Advance(revisionRetryInterval)
	}

	require.Equal(t, maxRevisionAttempts, f.count(http.MethodGet, kindBlob))
	require.Equal(t, 3*maxRevisionAttempts, f.gets())
	require.Equal(t, maxRevisionAttempts+5, f.heads())
}

// TestSettleRevisionAfterEviction: an interrupted lookup whose entry was
// evicted meanwhile must not remove the entry that took its key.
func TestSettleRevisionAfterEviction(t *testing.T) {
	r, err := New(testLogger(), Options{})
	require.NoError(t, err)

	stale, owner := r.claimRevision("k")
	require.True(t, owner)

	// Evicted and claimed again by somebody else.
	delete(r.revisions, "k")
	r.revisionOrder = nil

	current, owner := r.claimRevision("k")
	require.True(t, owner)

	r.settleRevision("k", stale, "", true, true)
	require.Same(t, current, r.revisions["k"])
	require.Equal(t, []string{"k"}, r.revisionOrder)

	// An interrupted entry that is not the newest in the order.
	_, owner = r.claimRevision("later")
	require.True(t, owner)

	r.settleRevision("k", current, "", true, true)
	require.NotContains(t, r.revisions, "k")
	require.Equal(t, []string{"later"}, r.revisionOrder)
}
