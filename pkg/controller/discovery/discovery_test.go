package discovery

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	image    = "ethpandaops/geth:master"
	interval = 5 * time.Minute
)

// fakeRegistry answers lookups from a map and counts them.
type fakeRegistry struct {
	mu      sync.Mutex
	digests map[string]string
	err     error
	asked   []string
}

func (f *fakeRegistry) Resolve(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.asked = append(f.asked, ref)

	return f.digests[ref], f.err
}

func (f *fakeRegistry) set(ref, digest string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.digests[ref], f.err = digest, err
}

// fixture is a tracker that is stepped by hand on a clock that is set.
type fixture struct {
	t   *tracker
	reg *fakeRegistry
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	log := logrus.New()
	log.SetOutput(io.Discard)

	f := &fixture{
		reg: &fakeRegistry{digests: map[string]string{image: "sha256:one"}},
		now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}

	tr, ok := NewTracker(log, f.reg, interval, func() time.Time { return f.now }).(*tracker)
	require.True(t, ok)

	f.t = tr

	return f
}

// step looks up every tag that is due and returns how many were.
func (f *fixture) step() int {
	n := 0

	for {
		key, _ := f.t.nextDue()
		if key == "" {
			return n
		}

		f.t.lookup(context.Background(), key)

		n++
	}
}

func TestATagIsLookedUpOncePerInterval(t *testing.T) {
	f := newFixture(t)

	assert.Empty(t, f.t.Sync("a", []string{image}, interval), "not looked up yet")
	assert.Equal(t, 1, f.step())

	checked := f.now
	want := []protocol.TagDigest{{Ref: image, Digest: "sha256:one", CheckedAt: checked}}
	assert.Equal(t, want, f.t.Sync("a", []string{image}, interval))

	// Another host writes the same tag differently and gets its spelling back.
	other := "docker.io/ethpandaops/geth:master"
	assert.Equal(t, []protocol.TagDigest{{Ref: other, Digest: "sha256:one", CheckedAt: checked}},
		f.t.Sync("b", []string{other}, interval))

	f.now = f.now.Add(interval - time.Second)
	assert.Zero(t, f.step(), "within the interval")

	f.reg.set(image, "sha256:two", nil)

	f.now = f.now.Add(time.Second)
	assert.Equal(t, 1, f.step())
	assert.Equal(t, []protocol.TagDigest{{Ref: image, Digest: "sha256:two", CheckedAt: f.now}},
		f.t.Sync("a", []string{image}, interval))
	assert.Equal(t, []string{image, image}, f.reg.asked, "one lookup for both hosts")
}

func TestAFailedLookupKeepsTheDigestAndIsRetriedSooner(t *testing.T) {
	f := newFixture(t)

	f.t.Sync("a", []string{image}, interval)
	f.step()

	f.reg.set(image, "", errors.New(strings.Repeat("x", 2*maxErrorLength)))

	f.now = f.now.Add(interval)
	require.Equal(t, 1, f.step())

	got := f.t.Sync("a", []string{image}, interval)
	require.Len(t, got, 1)
	assert.Equal(t, "sha256:one", got[0].Digest)
	assert.Equal(t, f.now, got[0].CheckedAt)
	assert.Len(t, got[0].Error, maxErrorLength)

	f.now = f.now.Add(retryAfter - time.Second)
	assert.Zero(t, f.step())

	f.reg.set(image, "sha256:one", nil)

	f.now = f.now.Add(time.Second)
	assert.Equal(t, 1, f.step())
	assert.Empty(t, f.t.Sync("a", []string{image}, interval)[0].Error)

	// A registry that answers no digest failed.
	f.reg.set(image, "", nil)

	f.now = f.now.Add(interval)
	f.step()
	assert.Equal(t, "registry returned no digest", f.t.Sync("a", []string{image}, interval)[0].Error)
}

func TestOnlyTagReferencesWithinTheBoundsAreFollowed(t *testing.T) {
	f := newFixture(t)

	tags := make([]string, 0, protocol.MaxTags+4)
	tags = append(tags,
		"ethpandaops/geth@sha256:"+strings.Repeat("a", 64),
		"ethpandaops/"+strings.Repeat("g", maxRefLength)+":master",
		"not a reference")

	for range protocol.MaxTags - 3 {
		tags = append(tags, image)
	}

	tags = append(tags, "ethpandaops/late:master")

	f.t.Sync("a", tags, interval)
	assert.Equal(t, 1, f.step())
	assert.Equal(t, []string{image}, f.reg.asked)
}

func TestATagNoHostListsIsForgotten(t *testing.T) {
	f := newFixture(t)

	f.t.Sync("a", []string{image}, interval)
	f.step()

	f.now = f.now.Add(forgetAfter + time.Second)
	assert.Zero(t, f.step())
	assert.Empty(t, f.t.tags)
	assert.Empty(t, f.t.Sync("a", []string{image}, interval), "followed anew")
}

func TestTheShorterIntervalOfASyncCounts(t *testing.T) {
	f := newFixture(t)

	f.t.Sync("a", []string{image}, interval)
	f.step()

	f.now = f.now.Add(time.Minute)
	f.t.Sync("a", []string{image}, time.Minute)
	assert.Equal(t, 1, f.step())
}

func TestRefreshLooksEveryTagUpNow(t *testing.T) {
	f := newFixture(t)

	f.t.Sync("a", []string{image}, interval)
	f.step()

	f.now = f.now.Add(refreshFloor - time.Second)
	assert.Equal(t, Refreshed{Recent: 1, Next: f.now.Add(time.Second)}, f.t.Refresh(), "just looked up")
	assert.Zero(t, f.step())

	f.reg.set(image, "sha256:two", nil)

	f.now = f.now.Add(time.Second)
	assert.Equal(t, Refreshed{Tags: 1}, f.t.Refresh())
	assert.Equal(t, Refreshed{Tags: 1}, f.t.Refresh(), "still waiting for its lookup")
	assert.Equal(t, 1, f.step())
	assert.Equal(t, "sha256:two", f.t.Sync("a", []string{image}, interval)[0].Digest)
	assert.Zero(t, f.step(), "once")
}

func TestStartLooksUpAndStopEnds(t *testing.T) {
	f := newFixture(t)

	require.NoError(t, f.t.Start(t.Context()))
	require.Error(t, f.t.Start(t.Context()))

	f.t.Sync("a", []string{image}, interval)

	require.Eventually(t, func() bool {
		return len(f.t.Sync("a", []string{image}, interval)) == 1
	}, 5*time.Second, time.Millisecond)

	require.NoError(t, f.t.Stop())
	require.NoError(t, f.t.Stop())
}
