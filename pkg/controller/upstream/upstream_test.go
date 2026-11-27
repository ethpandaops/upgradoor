package upstream

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
)

const (
	digestOne = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestTwo = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	digestOld = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

	imageLabelled = "ethpandaops/nimbus-eth2:pq-devnet-0"
	imageBare     = "ethpandaops/grandine:pq-devnet-0"
)

var errAbsent = errors.New("no such tag")

// fakes is the registry, the lookups and the remote of a test.
type fakes struct {
	mu     sync.Mutex
	tags   []discovery.Tag
	labels map[string]map[string]string // by digest
	digest map[string]string            // by reference
	heads  map[string]map[string]string // by repository
	failed error
	// refused fails every Resolve, as a rate limit does.
	refused error
	asked   []string
	reads   int
}

func (f *fakes) Tags() []discovery.Tag {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]discovery.Tag(nil), f.tags...)
}

func (f *fakes) Resolve(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.asked = append(f.asked, ref)

	if f.refused != nil {
		return "", f.refused
	}

	if digest, ok := f.digest[ref]; ok {
		return digest, nil
	}

	return "", errAbsent
}

func (f *fakes) Labels(_ context.Context, _, digest string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reads++

	return f.labels[digest], nil
}

func (f *fakes) Heads(_ context.Context, repo string, _ []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.heads[repo], f.failed
}

func (f *fakes) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn()
}

func newTracker(t *testing.T, f *fakes, changed *atomic.Int32) *tracker {
	t.Helper()

	log := logrus.New()
	log.SetOutput(io.Discard)

	tr, ok := New(log, Options{
		Lookups: f, Registry: f, Remote: f,
		NotFound: func(err error) bool { return errors.Is(err, errAbsent) },
		Changed:  func() { changed.Add(1) },
		Now:      time.Now,
	}).(*tracker)
	require.True(t, ok)

	return tr
}

func TestALabelledImageIsComparedWithTheHead(t *testing.T) {
	f := &fakes{
		tags: []discovery.Tag{{Ref: imageLabelled, Digest: digestOne}},
		labels: map[string]map[string]string{
			digestOne: {labelBuilderRepo: "status-im/nimbus-eth2", labelBuilderRef: "pq-devnet-0", labelBuilderCommit: "aaaaaaa"},
			digestTwo: {labelBuilderRepo: "status-im/nimbus-eth2", labelBuilderRef: "pq-devnet-0", labelBuilderCommit: "bbbbbbb"},
		},
		heads: map[string]map[string]string{"status-im/nimbus-eth2": {"pq-devnet-0": commitA}},
	}

	var changed atomic.Int32

	tr := newTracker(t, f, &changed)

	require.Nil(t, tr.Status(imageLabelled))

	tr.round(t.Context())
	require.Equal(t, &Status{Repo: "status-im/nimbus-eth2", Ref: "pq-devnet-0", Commit: "aaaaaaa", Head: commitA},
		tr.Status("docker.io/"+imageLabelled))
	require.EqualValues(t, 1, changed.Load())

	// Nothing moved: nothing is said, and the labels are not read again.
	tr.round(t.Context())
	require.EqualValues(t, 1, changed.Load())
	require.Equal(t, 1, f.reads)

	// The branch moves: a build is due.
	f.set(func() { f.heads["status-im/nimbus-eth2"]["pq-devnet-0"] = commitB })
	tr.round(t.Context())
	require.Equal(t, &Status{
		Repo: "status-im/nimbus-eth2", Ref: "pq-devnet-0", Commit: "aaaaaaa", Head: commitB, Pending: true,
	}, tr.Status(imageLabelled))
	require.EqualValues(t, 2, changed.Load())

	// The tag moves to the image of the new head.
	f.set(func() { f.tags[0].Digest = digestTwo })
	tr.round(t.Context())
	require.Equal(t, &Status{Repo: "status-im/nimbus-eth2", Ref: "pq-devnet-0", Commit: "bbbbbbb", Head: commitB},
		tr.Status(imageLabelled))
	require.Empty(t, f.asked)

	// The image the tag pointed at before is still known; one hosts run
	// that was never the tag's is read on request, once.
	f.set(func() { f.labels[digestOld] = map[string]string{labelBuilderCommit: "ccccccc"} })
	require.Equal(t, map[string]string{digestOne: "aaaaaaa", digestTwo: "bbbbbbb"},
		tr.Commits(imageLabelled, []string{digestOne, digestTwo, digestOld, ""}))
	tr.round(t.Context())
	require.Equal(t, map[string]string{digestOld: "ccccccc"}, tr.Commits(imageLabelled, []string{digestOld}))

	reads := f.reads
	tr.round(t.Context())
	require.Equal(t, reads, f.reads)
}

func TestAnImageWithoutLabelsIsComparedByTheBuildersTag(t *testing.T) {
	built := "docker.io/" + imageBare + "-aaaaaaa"
	f := &fakes{
		tags:   []discovery.Tag{{Ref: imageBare, Digest: digestOne}},
		digest: map[string]string{built: digestOne},
		heads:  map[string]map[string]string{"grandinetech/grandine": {"pq-devnet-0": commitA}},
	}

	var changed atomic.Int32

	tr := newTracker(t, f, &changed)

	tr.round(t.Context())
	require.Equal(t, &Status{Repo: "grandinetech/grandine", Ref: "pq-devnet-0", Commit: commitA, Head: commitA},
		tr.Status(imageBare))
	require.Equal(t, []string{built}, f.asked)

	// What was proven is not asked for again.
	tr.round(t.Context())
	require.Len(t, f.asked, 1)

	// The branch moves and its tag does not exist yet: pending, and the
	// image is still the one of the old head.
	f.set(func() { f.heads["grandinetech/grandine"]["pq-devnet-0"] = commitB })
	tr.round(t.Context())
	require.Equal(t, &Status{
		Repo: "grandinetech/grandine", Ref: "pq-devnet-0", Commit: commitA, Head: commitB, Pending: true,
	}, tr.Status(imageBare))

	// The build lands: both tags point at the new digest.
	f.set(func() {
		f.tags[0].Digest = digestTwo
		f.digest["docker.io/"+imageBare+"-bbbbbbb"] = digestTwo
	})
	tr.round(t.Context())
	require.Equal(t, &Status{Repo: "grandinetech/grandine", Ref: "pq-devnet-0", Commit: commitB, Head: commitB},
		tr.Status(imageBare))

	// What was proven of the older image is still known.
	require.Equal(t, map[string]string{digestOne: commitA, digestTwo: commitB},
		tr.Commits(imageBare, []string{digestOne, digestTwo}))

	// A tag that moved to an image nobody can place: its commit is unknown.
	f.set(func() { f.tags[0].Digest = digestOne; delete(f.digest, built) })
	tr.round(t.Context())
	require.Equal(t, &Status{Repo: "grandinetech/grandine", Ref: "pq-devnet-0", Head: commitB, Pending: true},
		tr.Status(imageBare))
}

func TestARegistryThatFailsIsLeftAloneForAWhile(t *testing.T) {
	f := &fakes{
		tags:    []discovery.Tag{{Ref: imageBare, Digest: digestOne}},
		digest:  map[string]string{"docker.io/" + imageBare + "-aaaaaaa": digestOne},
		heads:   map[string]map[string]string{"grandinetech/grandine": {"pq-devnet-0": commitA}},
		refused: errors.New("429 Too Many Requests"),
	}

	var changed atomic.Int32

	tr := newTracker(t, f, &changed)
	now := time.Now()
	tr.opts.Now = func() time.Time { return now }

	// Nothing is claimed of an image that could not be checked.
	want := &Status{Repo: "grandinetech/grandine", Ref: "pq-devnet-0", Head: commitA, Error: "429 Too Many Requests"}

	tr.round(t.Context())
	require.Equal(t, want, tr.Status(imageBare))

	tr.round(t.Context())
	require.Equal(t, want, tr.Status(imageBare))
	require.Len(t, f.asked, 1)

	// It is asked again after the pause.
	f.set(func() { f.refused = nil })

	now = now.Add(registryRetry)
	tr.round(t.Context())
	require.Equal(t, &Status{Repo: "grandinetech/grandine", Ref: "pq-devnet-0", Commit: commitA, Head: commitA},
		tr.Status(imageBare))
	require.Len(t, f.asked, 2)
}

func TestFailuresAndUnknownSources(t *testing.T) {
	f := &fakes{
		tags: []discovery.Tag{
			{Ref: imageBare, Digest: digestOne},
			{Ref: "ethereum/client-go:stable", Digest: digestOne},
			{Ref: "ethpandaops/teku:pq-devnet-0"},
		},
		heads:  map[string]map[string]string{},
		failed: errors.New("github.com/grandinetech/grandine: 503 Service Unavailable"),
	}

	var changed atomic.Int32

	tr := newTracker(t, f, &changed)

	tr.round(t.Context())
	require.Nil(t, tr.Status("ethereum/client-go:stable"))
	require.Nil(t, tr.Status("ethpandaops/teku:pq-devnet-0"))
	require.Equal(t, &Status{
		Repo: "grandinetech/grandine", Ref: "pq-devnet-0",
		Error: "github.com/grandinetech/grandine: 503 Service Unavailable",
	}, tr.Status(imageBare))

	// The repository has no such branch: nothing is pending.
	f.set(func() { f.failed = nil })
	tr.round(t.Context())
	require.Equal(t, &Status{Repo: "grandinetech/grandine", Ref: "pq-devnet-0"}, tr.Status(imageBare))

	// A tag nobody follows any more is forgotten.
	f.set(func() { f.tags = nil })
	tr.round(t.Context())
	require.Nil(t, tr.Status(imageBare))
}

func TestRefreshRunsARoundNow(t *testing.T) {
	f := &fakes{
		tags:   []discovery.Tag{{Ref: imageBare, Digest: digestOne}},
		heads:  map[string]map[string]string{"grandinetech/grandine": {"pq-devnet-0": commitA}},
		digest: map[string]string{"docker.io/" + imageBare + "-aaaaaaa": digestOne},
	}

	var changed atomic.Int32

	tr := newTracker(t, f, &changed)
	tr.opts.Interval = time.Hour

	require.NoError(t, tr.Start(t.Context()))
	require.Error(t, tr.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, tr.Stop()) })

	require.Eventually(t, func() bool { return tr.Status(imageBare) != nil }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, 1, tr.Refresh())
	require.NoError(t, tr.Stop())
	require.NoError(t, tr.Stop())
}
