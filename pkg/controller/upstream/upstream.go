// Package upstream compares the images of a devnet with their source: for
// every tag the hosts follow it finds the repository and the branch the
// image is built from, reads the branch's head once per interval and says
// whether the image the tag points at is built from it.
//
// Where an image comes from is read off its labels (the ones
// eth-client-docker-image-builder sets, or the OCI ones), off what was
// configured, or off the builder's table of repositories. What it was built
// from is the commit a label names; without one, the builder's
// "<tag>-<commit>" tag of the branch's head says so when it points at the
// digest the tag points at.
//
// The repository names come from image labels, so heads are only ever read
// from github.com. Everything is kept in memory.
package upstream

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

const (
	// DefaultInterval is how often the heads are read.
	DefaultInterval = time.Minute
	// RefreshFloor is the least time between two rounds, whoever asks.
	RefreshFloor = 10 * time.Second
	// maxWanted bounds the digests of one image that wait to be read, and
	// maxCommits the digests of one image whose commit is kept.
	maxWanted  = 16
	maxCommits = 64
	// quickRounds is how many rounds follow a Refresh at RefreshFloor.
	quickRounds = 4
	// roundTimeout bounds one round over every image.
	roundTimeout = 2 * time.Minute
	// labelsRetry is the pause after the labels of a digest could not be read.
	labelsRetry = 10 * time.Minute
	// registryRetry is the pause after the registry failed to say where the
	// builder's tag of a head points.
	registryRetry = 5 * time.Minute
	// maxErrorLength bounds an error that is shown.
	maxErrorLength = 300
)

// Lookups is the part of discovery.Tracker the tracker reads the followed
// tags from.
type Lookups interface {
	Tags() []discovery.Tag
}

// Registry is the part of registry.Resolver the tracker uses.
type Registry interface {
	Resolve(ctx context.Context, ref string) (string, error)
	Labels(ctx context.Context, ref, digest string) (map[string]string, error)
}

// Status is how the image of a tag stands against its source.
type Status struct {
	// Repo is the GitHub repository (owner/name) and Ref the branch or tag
	// the image is built from.
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	// Commit is the commit the image the tag points at was built from;
	// empty when that is not known.
	Commit string `json:"commit,omitempty"`
	// Head is the commit Ref is at; empty before the first answer and when
	// the repository has no such ref.
	Head string `json:"head,omitempty"`
	// Pending says that Head is known and the image is not built from it:
	// a build is due.
	Pending bool `json:"pending,omitempty"`
	// Error is why the newest check failed; the rest is then of the last
	// good one.
	Error string `json:"error,omitempty"`
}

// Tracker follows the sources of the images. It is safe for concurrent use.
type Tracker interface {
	// Start runs the checks until ctx ends or Stop.
	Start(ctx context.Context) error
	// Stop ends the checks and waits for the round in flight.
	Stop() error
	// Status says how the image of a tag reference, as a host writes it,
	// stands against its source; nil for an image whose source is not known
	// (yet).
	Status(image string) *Status
	// Commits says which commit the images of a tag reference at digests
	// were built from, by digest; a digest it does not know (yet) is left
	// out and read in the next round.
	Commits(image string, digests []string) map[string]string
	// Refresh asks for a round now and returns how many images are followed.
	Refresh() int
}

// Options configures a Tracker.
type Options struct {
	Lookups  Lookups
	Registry Registry
	Remote   Remote
	// NotFound reports whether an error of Registry says that a tag does
	// not exist (registry.IsNotFound).
	NotFound func(err error) bool
	// Sources are the configured sources by image repository (host/path),
	// as ParseSource returns them.
	Sources map[string]Source
	// Interval is the time between two rounds; zero means DefaultInterval.
	Interval time.Duration
	// Changed is called, without a lock held, when a Status changed.
	Changed func()
	Now     func() time.Time
}

// image is what the tracker knows of one followed tag.
type image struct {
	ref    imageref.Reference
	raw    string
	digest string
	// labelled says that the labels of digest were read (or, until retryAt,
	// that they could not be).
	labelled bool
	retryAt  time.Time
	labels   map[string]string
	// builtHead is the head whose "<tag>-<commit>" tag pointed at
	// builtDigest.
	builtHead   string
	builtDigest string
	// checkAt is when the builder's tag is asked for again after the
	// registry failed to answer.
	checkAt time.Time
	status  *Status
	// commits is the commit of every digest that was looked at; empty for
	// an image that does not say.
	commits map[string]string
}

type tracker struct {
	log  logrus.FieldLogger
	opts Options
	wake chan struct{}

	// quick counts the rounds that follow each other at RefreshFloor: a
	// refresh is asked for when a registry lookup is, and its answer is
	// compared a round later.
	quick atomic.Int32

	// images is only touched by the goroutine of run.
	images map[string]*image

	mu       sync.Mutex
	statuses map[string]Status
	// commits is image.commits of every image; wanted the digests Commits
	// was asked for and did not know.
	commits map[string]map[string]string
	wanted  map[string][]string

	lifeMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ Tracker = (*tracker)(nil)

// New returns a tracker over opts.
//
//nolint:gocritic // hugeParam: Options by value, like the other services of the controller.
func New(log logrus.FieldLogger, opts Options) Tracker {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}

	if opts.Changed == nil {
		opts.Changed = func() {}
	}

	return &tracker{
		log:      log.WithField("package", "upstream"),
		opts:     opts,
		wake:     make(chan struct{}, 1),
		images:   make(map[string]*image, 16),
		statuses: make(map[string]Status, 16),
		commits:  make(map[string]map[string]string, 16),
		wanted:   make(map[string][]string, 16),
	}
}

// Start implements Tracker.
func (t *tracker) Start(ctx context.Context) error {
	t.lifeMu.Lock()
	defer t.lifeMu.Unlock()

	if t.cancel != nil {
		return errors.New("upstream: tracker already started")
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		t.run(runCtx)
	}()

	t.cancel, t.done = cancel, done

	return nil
}

// Stop implements Tracker.
func (t *tracker) Stop() error {
	t.lifeMu.Lock()
	defer t.lifeMu.Unlock()

	if t.cancel == nil {
		return nil
	}

	t.cancel()
	<-t.done

	t.cancel, t.done = nil, nil

	return nil
}

// Status implements Tracker.
func (t *tracker) Status(ref string) *Status {
	t.mu.Lock()
	defer t.mu.Unlock()

	status, ok := t.statuses[imageref.Normalize(ref)]
	if !ok {
		return nil
	}

	return &status
}

// Commits implements Tracker.
func (t *tracker) Commits(ref string, digests []string) map[string]string {
	key := imageref.Normalize(ref)

	t.mu.Lock()
	defer t.mu.Unlock()

	known := t.commits[key]
	out := make(map[string]string, len(digests))

	for _, digest := range digests {
		commit, ok := known[digest]

		switch {
		case commit != "":
			out[digest] = commit
		case !ok && digest != "" && len(t.wanted[key]) < maxWanted && !slices.Contains(t.wanted[key], digest):
			t.wanted[key] = append(t.wanted[key], digest)
		}
	}

	return out
}

// Refresh implements Tracker.
func (t *tracker) Refresh() int {
	t.quick.Store(quickRounds)

	select {
	case t.wake <- struct{}{}:
	default:
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.statuses)
}

// run does a round per interval, and one when asked, until ctx ends.
func (t *tracker) run(ctx context.Context) {
	for ctx.Err() == nil {
		started := t.opts.Now()

		rctx, cancel := context.WithTimeout(ctx, roundTimeout)
		t.round(rctx)
		cancel()

		// Until the hosts have listed their tags there is nothing to compare.
		wait := t.opts.Interval
		if len(t.images) == 0 || t.quick.Add(-1) >= 0 {
			wait = min(wait, RefreshFloor)
		}

		next := time.NewTimer(wait)
		floor := time.NewTimer(max(RefreshFloor-t.opts.Now().Sub(started), 0))

		select {
		case <-ctx.Done():
		case <-next.C:
		case <-t.wake:
			select {
			case <-ctx.Done():
			case <-floor.C:
			}
		}

		next.Stop()
		floor.Stop()
	}
}

// round brings every followed tag up to date.
func (t *tracker) round(ctx context.Context) {
	t.follow(ctx)
	t.readWanted(ctx)

	// One request per repository, for all of its refs.
	refs := make(map[string][]string, len(t.images))

	for _, img := range t.images {
		if s := img.status; s != nil && !slices.Contains(refs[s.Repo], s.Ref) {
			refs[s.Repo] = append(refs[s.Repo], s.Ref)
		}
	}

	for _, repo := range slices.Sorted(maps.Keys(refs)) {
		slices.Sort(refs[repo])

		heads, err := t.opts.Remote.Heads(ctx, repo, refs[repo])
		if ctx.Err() != nil {
			return
		}

		for _, key := range slices.Sorted(maps.Keys(t.images)) {
			img := t.images[key]
			if img.status == nil || img.status.Repo != repo {
				continue
			}

			if err != nil {
				t.fail(img, err)

				continue
			}

			t.compare(ctx, img, heads[img.status.Ref])
		}
	}

	t.publish()
}

// follow takes the followed tags from the lookups, forgets the others and
// reads the labels of a digest it has not seen.
func (t *tracker) follow(ctx context.Context) {
	seen := make(map[string]struct{}, len(t.images))

	for _, tag := range t.opts.Lookups.Tags() {
		parsed, err := imageref.Parse(tag.Ref)
		if err != nil || parsed.Tag == "" || parsed.Digest != "" || tag.Digest == "" {
			continue
		}

		key := parsed.String()
		seen[key] = struct{}{}

		img := t.images[key]
		if img == nil {
			img = &image{ref: parsed, raw: tag.Ref}
			t.images[key] = img
		}

		if img.digest != tag.Digest {
			img.digest, img.labelled, img.labels = tag.Digest, false, nil
		}

		if !img.labelled && !t.opts.Now().Before(img.retryAt) {
			labels, err := t.opts.Registry.Labels(ctx, img.raw, img.digest)
			if ctx.Err() != nil {
				return
			}

			if err != nil {
				t.log.WithError(err).WithField("image", img.raw).Warn("reading the image labels failed")

				img.retryAt = t.opts.Now().Add(labelsRetry)
			}

			img.labelled, img.labels = err == nil, labels
		}

		source, ok := sourceOf(parsed, img.labels, t.opts.Sources)
		if !ok {
			img.status = nil

			continue
		}

		if img.status == nil || img.status.Repo != source.Repo || img.status.Ref != source.Ref {
			img.status = &Status{Repo: source.Repo, Ref: source.Ref}
		}

		if source.Commit != "" {
			img.status.Commit = source.Commit
		}
	}

	for key := range t.images {
		if _, ok := seen[key]; !ok {
			delete(t.images, key)
		}
	}
}

// readWanted reads the commit of the digests Commits was asked for: the
// digests hosts still run after the tag moved on. An image that cannot be
// read, or names no commit, is noted as such and not read again.
func (t *tracker) readWanted(ctx context.Context) {
	t.mu.Lock()
	wanted := t.wanted
	t.wanted = make(map[string][]string, len(wanted))
	t.mu.Unlock()

	for _, key := range slices.Sorted(maps.Keys(wanted)) {
		img := t.images[key]
		if img == nil {
			continue
		}

		for _, digest := range wanted[key] {
			if _, known := img.commits[digest]; known {
				continue
			}

			labels, err := t.opts.Registry.Labels(ctx, img.raw, digest)
			if ctx.Err() != nil {
				return
			}

			commit := ""

			if err == nil {
				source, _ := sourceOf(img.ref, labels, t.opts.Sources)
				commit = source.Commit
			}

			img.note(digest, commit)
		}
	}
}

// note keeps the commit of a digest; one that is known is not forgotten for
// an image that does not say.
func (img *image) note(digest, commit string) {
	if img.commits == nil || len(img.commits) >= maxCommits {
		img.commits = make(map[string]string, 4)
	}

	if commit != "" || img.commits[digest] == "" {
		img.commits[digest] = commit
	}
}

// compare sets the status of an image against the head of its ref; an empty
// head means the repository has no such ref.
func (t *tracker) compare(ctx context.Context, img *image, head string) {
	s := img.status
	was, failure := s.Pending, s.Error
	s.Head, s.Error = head, ""

	defer func() {
		if s.Pending && !was {
			t.log.WithFields(logrus.Fields{
				"image": img.raw, "source": s.Repo + "@" + s.Ref, "head": head, "built": s.Commit,
			}).Info("the source is ahead of the image")
		}
	}()

	if head == "" {
		s.Pending = false

		return
	}

	// A label names the commit.
	if source, _ := sourceOf(img.ref, img.labels, t.opts.Sources); source.Commit != "" {
		s.Commit = source.Commit
		s.Pending = !strings.HasPrefix(head, source.Commit)
		img.note(img.digest, source.Commit)

		return
	}

	// Without one, the builder's tag of the head says whether it is built.
	if img.builtDigest != img.digest || img.builtHead != head {
		// A registry that fails (a rate limit) is left alone for a while.
		if t.opts.Now().Before(img.checkAt) {
			s.Error = failure

			return
		}

		built := img.ref
		built.Tag += "-" + head[:shortCommit]

		digest, err := t.opts.Registry.Resolve(ctx, built.String())

		switch {
		case ctx.Err() != nil:
			return
		case err == nil && digest == img.digest:
			img.builtHead, img.builtDigest = head, img.digest
		case err == nil || t.opts.NotFound(err):
		default:
			img.checkAt = t.opts.Now().Add(registryRetry)
			s.Error = failure

			t.fail(img, err)

			return
		}
	}

	s.Commit = ""
	if img.builtDigest == img.digest {
		s.Commit = img.builtHead
		img.note(img.digest, img.builtHead)
	}

	s.Pending = s.Commit != head
}

// fail notes a failed check on the image's status.
func (t *tracker) fail(img *image, err error) {
	msg := err.Error()
	if len(msg) > maxErrorLength {
		msg = msg[:maxErrorLength]
	}

	// Logged once per failure, not once per round.
	if msg != img.status.Error {
		t.log.WithError(err).WithField("image", img.raw).Warn("checking the image's source failed")
	}

	img.status.Error = msg
}

// publish hands the statuses to the readers and says so when one changed.
func (t *tracker) publish() {
	statuses := make(map[string]Status, len(t.images))

	for key, img := range t.images {
		if img.status != nil {
			statuses[key] = *img.status
		}
	}

	commits := make(map[string]map[string]string, len(t.images))
	for key, img := range t.images {
		commits[key] = maps.Clone(img.commits)
	}

	t.mu.Lock()
	changed := !maps.Equal(statuses, t.statuses) ||
		!maps.EqualFunc(commits, t.commits, maps.Equal[map[string]string])
	t.statuses, t.commits = statuses, commits
	t.mu.Unlock()

	if changed {
		t.opts.Changed()
	}
}
