package registry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// pullTimeout bounds one fallback pull.
	pullTimeout = 10 * time.Minute
	// pullRetry is the pause after a failed pull of a tag.
	pullRetry = 5 * time.Minute
	// maxIdle bounds how long the goroutine sleeps without looking again.
	maxIdle = time.Minute
)

// Images is the machine's local images and pull; host.Host satisfies it.
type Images interface {
	// LocalImage returns the local image's registry digest ("" when absent)
	// and revision label.
	LocalImage(ctx context.Context, ref string) (digest, revision string, err error)
	// Pull pulls ref by its tag and returns what LocalImage then would.
	Pull(ctx context.Context, ref string) (digest, revision string, err error)
}

// Tracker keeps what the controller says the followed tags point at and
// looks at the local images in one goroutine, one at a time, off the sync
// loop. It never asks a registry and pulls only the tags of the fallback,
// while pull is on: what they pull is where the tag points. Every other pull
// is a lease's, which tells the tracker how it went. All methods but Start
// and Stop return at once.
type Tracker interface {
	// Start runs the goroutine until ctx ends or Stop.
	Start(ctx context.Context) error
	// Stop ends the goroutine, stopping a pull in flight, and waits for it.
	// Safe after a failed Start and more than once.
	Stop() error

	// Track sets the tags to follow: the reference as written by normalised
	// reference. An unlisted tag is forgotten with its check.
	Track(tags map[string]string)
	// SetDigests takes in the controller's lookups; only newer ones count.
	SetDigests(digests []protocol.TagDigest)
	// NotePull takes in how a lease's pull of a followed tag went: after a
	// failure the tag's check says PullFailed for a while, after a success
	// the local image is looked at again.
	NotePull(ref string, err error)
	// SetFallback sets the followed tags (by normalised reference) that are
	// pulled once per interval while the controller cannot be reached, each
	// first at a random point of it; nil ends the fallback and its pull.
	SetFallback(tags map[string]string, interval time.Duration)
	// Checks returns a copy of the check of every tag the controller has
	// looked up, by normalised reference.
	Checks() map[string]protocol.RegistryCheck
	// Resolve returns the digest of the controller's newest good lookup of a
	// followed tag.
	Resolve(ctx context.Context, ref string) (string, error)
}

type tracker struct {
	log    logrus.FieldLogger
	images Images
	now    func() time.Time
	pull   bool

	// wake is signalled, never blocking, when something may have become due.
	wake chan struct{}

	mu   sync.Mutex
	tags map[string]*trackedTag // by normalised reference
	// pulling is the pull in flight, nil when there is none.
	pulling *activePull
	// interval is the time between two fallback pulls of a tag.
	interval time.Duration

	lifeMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

type trackedTag struct {
	ref string
	// check is the controller's newest lookup with what the local images
	// say; its Error is put together by Checks.
	check protocol.RegistryCheck
	// checked: the controller has looked the tag up.
	checked bool
	// lookupErr is why the controller's newest lookup failed; localErr is
	// the error of the last look at the local images or of the last
	// fallback pull.
	lookupErr, localErr string
	// local asks for a look at the local images.
	local bool
	// pullErr is why a lease's last pull failed; it counts until pullRetryAt.
	pullErr     string
	pullRetryAt time.Time
	// own: the tag is in the fallback, pulled next at nextPull.
	own      bool
	nextPull time.Time
}

// activePull is the fallback pull in flight.
type activePull struct {
	key, ref string
	cancel   context.CancelFunc
}

var _ Tracker = (*tracker)(nil)

// NewTracker returns a tracker; with pull false it never pulls.
func NewTracker(log logrus.FieldLogger, images Images, pull bool, now func() time.Time) Tracker {
	return &tracker{
		log:    log.WithFields(logrus.Fields{"package": logPackage, "component": "tracker"}),
		images: images,
		now:    now,
		pull:   pull,
		wake:   make(chan struct{}, 1),
		tags:   make(map[string]*trackedTag, 8),
	}
}

// Start implements Tracker.
func (t *tracker) Start(ctx context.Context) error {
	t.lifeMu.Lock()
	defer t.lifeMu.Unlock()

	if t.cancel != nil {
		return errors.New("registry: tracker already started")
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

	// A pull's context derives from the run context: this stops it too.
	t.cancel()
	<-t.done

	t.cancel, t.done = nil, nil

	return nil
}

// Track implements Tracker.
func (t *tracker) Track(tags map[string]string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for key, ref := range tags {
		cur, ok := t.tags[key]
		if !ok {
			cur = &trackedTag{}
			t.tags[key] = cur
		}

		cur.ref = ref
	}

	for key := range t.tags {
		if _, ok := tags[key]; !ok {
			delete(t.tags, key)
		}
	}
}

// SetDigests implements Tracker; revision and pulled are cleared when the
// digest changes. A failed lookup keeps the last good digest.
func (t *tracker) SetDigests(digests []protocol.TagDigest) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, d := range digests {
		tag, ok := t.tags[imageref.Normalize(d.Ref)]
		if !ok || (tag.checked && !d.CheckedAt.After(tag.check.CheckedAt)) {
			continue
		}

		if d.Digest != "" && d.Digest != tag.check.Digest {
			t.log.WithFields(logrus.Fields{
				"image": tag.ref, "digest": d.Digest, "previous": tag.check.Digest,
			}).Info("tag points at a new digest")

			tag.check.Digest, tag.check.Revision, tag.check.Pulled = d.Digest, "", false
			tag.localErr, tag.pullErr, tag.pullRetryAt = "", "", time.Time{}
		}

		tag.check.CheckedAt, tag.lookupErr = d.CheckedAt, d.Error
		tag.checked = true
		tag.local = true

		t.signal()
	}
}

// NotePull implements Tracker.
func (t *tracker) NotePull(ref string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tag, ok := t.tags[imageref.Normalize(ref)]
	if !ok {
		return
	}

	if err != nil {
		tag.pullErr, tag.pullRetryAt = "pull: "+err.Error(), t.now().Add(pullRetry)

		return
	}

	tag.pullErr, tag.pullRetryAt = "", time.Time{}
	tag.local = true

	t.signal()
}

// SetFallback implements Tracker.
func (t *tracker) SetFallback(tags map[string]string, interval time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.interval = interval
	now := t.now()

	for key, tag := range t.tags {
		_, on := tags[key]
		if on && !tag.own && interval > 0 {
			// Hosts lose the controller together: they must not pull together.
			tag.nextPull = now.Add(rand.N(interval)) //nolint:gosec // a spread, not a secret
		}

		tag.own = on && interval > 0
	}

	if p := t.pulling; p != nil {
		if tag, ok := t.tags[p.key]; !ok || !tag.own {
			p.cancel()
		}
	}

	t.signal()
}

// Checks implements Tracker.
func (t *tracker) Checks() map[string]protocol.RegistryCheck {
	t.mu.Lock()
	defer t.mu.Unlock()

	checks := make(map[string]protocol.RegistryCheck, len(t.tags))
	now := t.now()

	for key, tag := range t.tags {
		if !tag.checked {
			continue
		}

		check := tag.check
		if tag.lookupErr != "" {
			check.Error = "registry: " + tag.lookupErr
		}

		check.Error = cmp.Or(tag.localErr, check.Error)

		if now.Before(tag.pullRetryAt) {
			check.PullFailed, check.Error = true, tag.pullErr
		}

		checks[key] = check
	}

	return checks
}

// Resolve implements Tracker.
func (t *tracker) Resolve(_ context.Context, ref string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tag, ok := t.tags[imageref.Normalize(ref)]
	if !ok || tag.check.Digest == "" {
		return "", fmt.Errorf("the controller has not said where %s points", ref)
	}

	return tag.check.Digest, nil
}

// pullingNow returns the reference of the fallback pull in flight, or "".
func (t *tracker) pullingNow() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.pulling != nil {
		return t.pulling.ref
	}

	return ""
}

// signal wakes the goroutine; the caller holds t.mu.
func (t *tracker) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// run handles the due tags, one at a time, until ctx ends.
func (t *tracker) run(ctx context.Context) {
	for ctx.Err() == nil {
		if key := t.nextDue(); key != "" {
			t.checkTag(ctx, key)

			continue
		}

		timer := time.NewTimer(maxIdle)

		select {
		case <-ctx.Done():
		case <-t.wake:
		case <-timer.C:
		}

		timer.Stop()
	}
}

// ownPull reports whether a fallback pull of the tag is due. The caller
// holds t.mu.
func (t *tracker) ownPull(tag *trackedTag, now time.Time) bool {
	return t.pull && tag.own && !now.Before(tag.nextPull)
}

// nextDue returns the tag that is due (the first by key), or "".
func (t *tracker) nextDue() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()

	keys := make([]string, 0, len(t.tags))
	for key := range t.tags {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		if tag := t.tags[key]; tag.local || t.ownPull(tag, now) {
			return key
		}
	}

	return ""
}

// checkTag serves one due tag without the lock held meanwhile: a fallback
// pull, or a look at the local image. The look counts only for the entry
// and the digest it began with.
func (t *tracker) checkTag(ctx context.Context, key string) {
	t.mu.Lock()

	tag, ok := t.tags[key]
	if !ok {
		t.mu.Unlock()

		return
	}

	ref, digest := tag.ref, tag.check.Digest
	own := t.ownPull(tag, t.now())

	if !own {
		tag.local = false
	}

	t.mu.Unlock()

	if own {
		t.pullOwn(ctx, t.log.WithField("image", ref), key, ref, tag)

		return
	}

	if digest == "" {
		return
	}

	got, revision, err := t.images.LocalImage(ctx, ref)
	if ctx.Err() != nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.tags[key] != tag || tag.check.Digest != digest {
		return
	}

	if err != nil {
		tag.check.Pulled, tag.localErr = false, "local image: "+err.Error()

		return
	}

	tag.check.Pulled, tag.localErr = got == digest, ""
	if tag.check.Pulled {
		tag.check.Revision = revision
	}
}

// pullOwn pulls a fallback tag and takes what it pulled as where the tag
// points. The result counts only while the entry is still in the fallback.
func (t *tracker) pullOwn(ctx context.Context, log logrus.FieldLogger, key, ref string, tag *trackedTag) {
	pctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	t.mu.Lock()

	if t.tags[key] != tag || !tag.own {
		t.mu.Unlock()

		return
	}

	t.pulling = &activePull{key: key, ref: ref, cancel: cancel}
	t.mu.Unlock()

	got, revision, err := t.images.Pull(pctx, ref)
	if err == nil && got == "" {
		err = errors.New("the pulled image has no registry digest")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.pulling = nil

	if ctx.Err() != nil || t.tags[key] != tag || !tag.own {
		return
	}

	now := t.now()

	if err != nil {
		log.WithError(err).Warn("fallback pull failed")

		tag.localErr = "pull: " + err.Error()
		tag.nextPull = now.Add(min(t.interval, pullRetry))

		return
	}

	if got != tag.check.Digest {
		log.WithFields(logrus.Fields{"digest": got, "previous": tag.check.Digest}).
			Info("fallback pull: tag points at a new digest")
	}

	tag.check = protocol.RegistryCheck{Digest: got, Revision: revision, Pulled: true, CheckedAt: now}
	tag.checked, tag.lookupErr, tag.localErr = true, "", ""
	tag.pullErr, tag.pullRetryAt = "", time.Time{}
	tag.nextPull = now.Add(t.interval)
}
