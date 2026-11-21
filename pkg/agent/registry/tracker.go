package registry

import (
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
	// lookupTimeout bounds one registry lookup.
	lookupTimeout = time.Minute
	// pullTimeout bounds one image pull.
	pullTimeout = 30 * time.Minute
	// pullRetry is the pause after a failed pull of a tag.
	pullRetry = time.Minute
	// pollJitter spreads the hosts' lookups of one tag over time.
	pollJitter = 0.2
	// maxIdle bounds how long the goroutine sleeps without looking again.
	maxIdle = time.Minute
	// minIdle keeps a tag that was due in the past from spinning the loop.
	minIdle = 10 * time.Millisecond
)

// Images is the machine's local images and pull; host.Host satisfies it.
type Images interface {
	// LocalImage returns the local image's registry digest ("" when absent)
	// and revision label.
	LocalImage(ctx context.Context, ref string) (digest, revision string, err error)
	// Pull pulls ref by its tag and returns what LocalImage then would.
	Pull(ctx context.Context, ref string) (digest, revision string, err error)
}

// Tag is one tag to follow; it has the fields of plan.Tag, which registry
// does not import.
type Tag struct {
	// Ref is the reference as written on the container or target.
	Ref string
	// InUse: a container runs the tag; otherwise it is always looked up on
	// the tracker's own schedule.
	InUse bool
}

// Tracker looks tags up and pulls images in one goroutine, one at a time,
// off the sync loop. An image is pulled only while pull is on, the window
// names the tag at exactly the digest of the tracker's check and the image
// is not local at it; it counts as pulled only at that digest. A failed
// lookup keeps the last good digest. All methods but Start and Stop return
// at once.
type Tracker interface {
	// Start runs the goroutine until ctx ends or Stop.
	Start(ctx context.Context) error
	// Stop ends the goroutine, stopping a pull in flight, and waits for it.
	// Safe after a failed Start and more than once.
	Stop() error

	// Track sets the tags to follow by normalised reference; an unlisted
	// tag is forgotten with its check.
	Track(tags map[string]Tag)
	// Lookup asks for a fresh lookup of the listed tracked tags.
	Lookup(refs []string)
	// Request asks for a fresh lookup of one tag for a roll, adding it when
	// untracked, and returns a ticket for Answered.
	Request(ref string) uint64
	// Answered returns the check answering a Request ticket: done once a
	// lookup begun after the request finished (a relay does not count);
	// gone when the tag is no longer tracked.
	Answered(ref string, ticket uint64) (check protocol.RegistryCheck, done, gone bool)
	// Recheck asks for a fresh lookup of every tag.
	Recheck()
	// Relay takes in the digests other hosts saw; only newer ones count.
	Relay(digests []protocol.TagDigest)
	// SetWindow sets the pull window (nil: none) and stops a pull it no
	// longer names.
	SetWindow(window *protocol.PullWindow)
	// SetFallback switches the own lookup schedule on or off for every tag.
	SetFallback(on bool)
	// Checks returns a copy of the last check of every checked tag, by
	// normalised reference.
	Checks() map[string]protocol.RegistryCheck
	// Pulling returns the references being pulled now.
	Pulling() []string
}

type tracker struct {
	log      logrus.FieldLogger
	images   Images
	resolver Resolver
	interval time.Duration
	now      func() time.Time
	pull     bool

	// wake is signalled, never blocking, when something may have become due.
	wake chan struct{}

	mu   sync.Mutex
	tags map[string]*trackedTag // by normalised reference
	// window is the digest each tag may be pulled at, by normalised reference.
	window   map[string]string
	fallback bool
	// pulling is the pull in flight, nil when there is none.
	pulling *activePull

	lifeMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

type trackedTag struct {
	ref   string
	inUse bool
	check protocol.RegistryCheck
	// checked: check holds a looked-up or relayed digest, or a failure.
	checked bool
	// next is when the tag is due on the tracker's own schedule.
	next time.Time
	// wantGen counts lookup requests (a Request's ticket); doneGen is the
	// count the last finished lookup began at. A relay touches neither, so
	// it never answers a roll.
	wantGen, doneGen uint64
	// local asks for a look at the local images only, after a relay.
	local bool
	// retryAt holds a pull back after a failure.
	retryAt time.Time
}

// activePull is the pull in flight; it keeps the reference so Pulling reports
// it after Track dropped the tag.
type activePull struct {
	key, ref, digest string
	cancel           context.CancelFunc
}

var _ Tracker = (*tracker)(nil)

// NewTracker returns a tracker in fallback; with pull false it only looks up.
func NewTracker(
	log logrus.FieldLogger, images Images, resolver Resolver, interval time.Duration, pull bool,
	now func() time.Time,
) Tracker {
	return &tracker{
		log:      log.WithFields(logrus.Fields{"package": logPackage, "component": "tracker"}),
		images:   images,
		resolver: resolver,
		interval: interval,
		now:      now,
		pull:     pull,
		wake:     make(chan struct{}, 1),
		tags:     make(map[string]*trackedTag, 8),
		window:   make(map[string]string, 2),
		fallback: true,
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
func (t *tracker) Track(tags map[string]Tag) {
	t.mu.Lock()
	defer t.mu.Unlock()

	added := false

	for key, tag := range tags {
		cur, ok := t.tags[key]
		if !ok {
			cur = &trackedTag{}
			t.tags[key] = cur
			added = true
		}

		cur.ref, cur.inUse = tag.Ref, tag.InUse
	}

	// A requested tag that is not listed goes too: Answered then says gone.
	for key := range t.tags {
		if _, ok := tags[key]; !ok {
			delete(t.tags, key)
		}
	}

	if added {
		t.signal()
	}
}

// Lookup implements Tracker.
func (t *tracker) Lookup(refs []string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, ref := range refs {
		if tag, ok := t.tags[imageref.Normalize(ref)]; ok {
			tag.wantGen++
		}
	}

	t.signal()
}

// Recheck implements Tracker.
func (t *tracker) Recheck() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, tag := range t.tags {
		tag.wantGen++
	}

	t.signal()
}

// Relay implements Tracker; revision and pulled are cleared when the digest
// changes.
func (t *tracker) Relay(digests []protocol.TagDigest) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, d := range digests {
		tag, ok := t.tags[imageref.Normalize(d.Ref)]
		if !ok || d.Digest == "" || (tag.checked && !d.CheckedAt.After(tag.check.CheckedAt)) {
			continue
		}

		if tag.check.Digest != d.Digest {
			tag.check.Revision, tag.check.Pulled = "", false
		}

		tag.check.Digest, tag.check.CheckedAt, tag.check.Error = d.Digest, d.CheckedAt, ""
		tag.checked = true
		tag.local = true
	}

	t.signal()
}

// SetWindow implements Tracker.
func (t *tracker) SetWindow(window *protocol.PullWindow) {
	t.mu.Lock()
	defer t.mu.Unlock()

	clear(t.window)

	if window != nil {
		for _, img := range window.Images {
			t.window[imageref.Normalize(img.Ref)] = img.Digest
		}
	}

	// A pull the window no longer names at its digest stops.
	if t.pulling != nil && t.window[t.pulling.key] != t.pulling.digest {
		t.pulling.cancel()
	}

	t.signal()
}

// SetFallback implements Tracker.
func (t *tracker) SetFallback(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.fallback != on {
		t.fallback = on
		t.signal()
	}
}

// Checks implements Tracker.
func (t *tracker) Checks() map[string]protocol.RegistryCheck {
	t.mu.Lock()
	defer t.mu.Unlock()

	checks := make(map[string]protocol.RegistryCheck, len(t.tags))

	for key, tag := range t.tags {
		if tag.checked {
			checks[key] = tag.check
		}
	}

	return checks
}

// Request implements Tracker.
func (t *tracker) Request(ref string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := imageref.Normalize(ref)

	tag, ok := t.tags[key]
	if !ok {
		tag = &trackedTag{ref: ref}
		t.tags[key] = tag
	}

	tag.wantGen++

	t.signal()

	return tag.wantGen
}

// Answered implements Tracker.
func (t *tracker) Answered(ref string, ticket uint64) (check protocol.RegistryCheck, done, gone bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tag, ok := t.tags[imageref.Normalize(ref)]
	if !ok {
		return protocol.RegistryCheck{}, false, true
	}

	if tag.doneGen < ticket {
		return protocol.RegistryCheck{}, false, false
	}

	return tag.check, true, false
}

// Pulling implements Tracker.
func (t *tracker) Pulling() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.pulling != nil {
		return []string{t.pulling.ref}
	}

	return nil
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
		key, wait := t.nextDue()
		if key != "" {
			t.checkTag(ctx, key)

			continue
		}

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-t.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// own reports whether the tag is looked up on the tracker's own schedule.
// The caller holds t.mu.
func (t *tracker) own(tag *trackedTag) bool {
	return t.fallback || !tag.inUse
}

// wantsPull reports whether the window names the tag at exactly the digest of
// the tracker's check, so the window alone cannot name a digest to pull. The
// caller holds t.mu.
func (t *tracker) wantsPull(key string, tag *trackedTag, now time.Time) bool {
	allowed := t.window[key]

	return t.pull && allowed != "" && tag.checked && allowed == tag.check.Digest &&
		!tag.check.Pulled && !now.Before(tag.retryAt)
}

// nextDue returns the tag that is due (the first by key), or the time to
// sleep until the next one is.
func (t *tracker) nextDue() (string, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var (
		now  = t.now()
		due  string
		wait = maxIdle
	)

	keys := make([]string, 0, len(t.tags))
	for key := range t.tags {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		tag := t.tags[key]

		switch {
		case tag.wantGen != tag.doneGen, tag.local, t.wantsPull(key, tag, now),
			t.own(tag) && !tag.next.After(now):
			if due == "" {
				due = key
			}
		case t.own(tag):
			wait = min(wait, tag.next.Sub(now))
		}
	}

	return due, max(wait, minIdle)
}

// checkTag looks one due tag up, pulls when the window allows and records the
// result, without the lock held while asking. The result goes only to the
// entry it began with, so a lookup begun before Track dropped and Request
// re-added the tag cannot answer the new request.
func (t *tracker) checkTag(ctx context.Context, key string) {
	t.mu.Lock()

	tag, ok := t.tags[key]
	if !ok {
		t.mu.Unlock()

		return
	}

	var (
		now      = t.now()
		ref      = tag.ref
		startGen = tag.wantGen
		lookup   = tag.wantGen != tag.doneGen || (t.own(tag) && !tag.next.After(now)) || !tag.checked
		result   = tag.check
	)

	tag.local = false

	t.mu.Unlock()

	log := t.log.WithField("image", ref)

	if lookup {
		t.lookup(ctx, log, ref, &result)
	}

	if ctx.Err() != nil {
		return
	}

	tried := false

	if result.Digest != "" {
		result.Pulled, tried = t.ensureLocal(ctx, log, key, ref, tag, &result)
	}

	if ctx.Err() != nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.tags[key] != tag {
		return
	}

	tag.check = result
	tag.checked = true

	if tried && !result.Pulled {
		tag.retryAt = t.now().Add(pullRetry)
	}

	if lookup {
		tag.doneGen = startGen
		tag.next = t.now().Add(t.jittered())
	}
}

// lookup records where the tag points now; a failure keeps the last good digest.
func (t *tracker) lookup(
	ctx context.Context, log logrus.FieldLogger, ref string, result *protocol.RegistryCheck,
) {
	digest, err := t.resolve(ctx, ref)
	if ctx.Err() != nil {
		return
	}

	result.CheckedAt = t.now()
	result.Error = ""

	switch {
	case err != nil:
		result.Error = "registry: " + err.Error()

		log.WithError(err).Warn("registry check failed")
	case digest != result.Digest:
		log.WithFields(logrus.Fields{"digest": digest, "previous": result.Digest}).
			Info("tag points at a new digest")

		// The revision of the new image is known once the image is local.
		result.Digest, result.Revision = digest, ""
	}
}

// resolve is one bounded lookup; a registry that answers no digest failed.
func (t *tracker) resolve(ctx context.Context, ref string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	digest, err := t.resolver.Resolve(rctx, ref)
	if err != nil {
		return "", err
	}

	if digest == "" {
		return "", fmt.Errorf("registry returned no digest for %s", ref)
	}

	return digest, nil
}

// ensureLocal reports whether the image at result.Digest is local, pulling it
// by tag when the window allows; tried says a pull was attempted. A pull
// counts only at exactly result.Digest, since the tag may have moved.
func (t *tracker) ensureLocal(
	ctx context.Context, log logrus.FieldLogger, key, ref string, tag *trackedTag,
	result *protocol.RegistryCheck,
) (local, tried bool) {
	digest, revision, err := t.images.LocalImage(ctx, ref)
	if err != nil {
		if result.Error == "" {
			result.Error = "local image: " + err.Error()
		}

		return false, false
	}

	if digest == result.Digest {
		result.Revision = revision

		return true, false
	}

	pctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	// Checked under the lock right before the pull, so a window changed
	// during the lookup is honoured; SetWindow stops the pull later.
	t.mu.Lock()

	if !t.pull || t.window[key] != result.Digest || t.tags[key] != tag {
		t.mu.Unlock()

		return false, false
	}

	t.pulling = &activePull{key: key, ref: ref, digest: result.Digest, cancel: cancel}
	t.mu.Unlock()

	log.WithField("digest", result.Digest).Info("pulling image in the pull window")

	got, revision, err := t.images.Pull(pctx, ref)

	t.mu.Lock()
	t.pulling = nil
	t.mu.Unlock()

	if err != nil {
		result.Error = "pull: " + err.Error()

		log.WithError(err).Warn("image pull failed")

		return false, true
	}

	if got != result.Digest {
		result.Error = fmt.Sprintf("pulled digest %s differs from registry digest %s", got, result.Digest)

		return false, true
	}

	result.Revision = revision

	log.WithField("digest", got).Info("image pulled")

	return true, true
}

// jittered returns the poll interval plus up to pollJitter of it.
func (t *tracker) jittered() time.Duration {
	//nolint:gosec // jitter only spreads load; it needs no cryptographic randomness
	return t.interval + time.Duration(rand.Float64()*pollJitter*float64(t.interval))
}
