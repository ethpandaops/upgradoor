// Package imagewatch follows the tags of the managed containers: it looks
// them up at the registry when the controller hands it the duty, takes the
// digests other hosts saw from the controller, and pulls images only in the
// pull windows the controller grants.
package imagewatch

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// ResolveTimeout bounds one registry lookup.
	ResolveTimeout = time.Minute
	// pullTimeout bounds one image pull.
	pullTimeout = 30 * time.Minute
	// pullRetry is the pause after a failed pull before the window is used
	// again.
	pullRetry = time.Minute
	// pollJitter is the share of the poll interval added at random.
	pollJitter = 0.2
	// maxWatchIdle bounds how long the watcher sleeps without looking again.
	maxWatchIdle = time.Minute
)

// Watcher follows the tags of the managed containers.
//
// It looks a tag up at the registry when the controller hands it the duty
// (Lookup), when a roll asks for it (Request), and on its own schedule only
// while the controller has not answered for a while (SetFallback) or for a
// tag no container of the host runs (the image of an override). What another
// host saw is relayed by the controller (Relay); the watcher then looks at
// its local images. It pulls only what the controller's pull window names
// (SetWindow), at the window's digest.
//
// It runs in its own goroutine because a pull can take minutes and the sync
// loop must not wait for it. Pulling never touches a running container.
type Watcher interface {
	// Run looks up and pulls what is due until ctx ends.
	Run(ctx context.Context)
	Track(tags map[string]Tag)
	Check(key string) (protocol.RegistryCheck, bool)
	RecheckAll()
	Lookup(refs []string)
	Relay(digests []protocol.TagDigest)
	SetWindow(window *protocol.PullWindow)
	SetFallback(on bool)
	Pulling() []string
	Request(key, ref string) uint64
	Result(key string, ticket uint64) (check protocol.RegistryCheck, done, gone bool)
}

// watcher is the Watcher.
type watcher struct {
	log      logrus.FieldLogger
	runtime  hostapi.ContainerRuntime
	registry hostapi.Registry
	interval time.Duration
	now      func() time.Time
	// pull is false on a host that never rolls: tags are resolved for the
	// report, images are not pulled.
	pull bool
	// changed is called when a lookup found a new digest.
	changed func()

	mu   sync.Mutex
	tags map[string]*watchedTag // by normalised reference
	wake chan struct{}
	// fallback: the controller has not answered for a while; every tag is
	// looked up on the watcher's own schedule.
	fallback bool
	// pulling is the tag being pulled, cancelPull stops it.
	pulling    string
	cancelPull context.CancelFunc
}

// watchedTag is one followed tag.
type watchedTag struct {
	// ref is the reference as written on the container.
	ref string
	// running are the digests of the containers on this tag; "" stands for
	// a container that is to move to the tag.
	running []string
	check   protocol.RegistryCheck
	checked bool
	// next is when the tag is due on the watcher's own schedule.
	next time.Time
	// wantGen counts lookup requests; doneGen is the request count the last
	// finished check had seen when it started.
	wantGen, doneGen uint64
	// local asks for a look at the local images only (a relayed digest).
	local bool
	// allowed is the digest the pull window lets the host pull; retryAt
	// holds a pull back after a failure.
	allowed string
	retryAt time.Time
}

var _ Watcher = (*watcher)(nil)

// New returns the watcher of a host. interval is the host's own lookup
// interval per tag; pull is false on a host that never rolls (tags are
// looked up for the report, images are not pulled).
func New(
	log logrus.FieldLogger,
	runtime hostapi.ContainerRuntime,
	registry hostapi.Registry,
	interval time.Duration,
	now func() time.Time,
	pull bool,
) Watcher {
	return newWatcher(log, runtime, registry, interval, now, pull)
}

func newWatcher(
	log logrus.FieldLogger,
	runtime hostapi.ContainerRuntime,
	registry hostapi.Registry,
	interval time.Duration,
	now func() time.Time,
	pull bool,
) *watcher {
	return &watcher{
		log:      log.WithField("component", "imagewatch"),
		runtime:  runtime,
		registry: registry,
		interval: interval,
		now:      now,
		pull:     pull,
		changed:  func() {},
		tags:     make(map[string]*watchedTag, 8),
		wake:     make(chan struct{}, 1),
		fallback: true,
	}
}

// Track sets the tags to follow: reference as written, by normalised
// reference, with the digests the containers on it run. Tags that are no
// longer listed are forgotten.
func (w *watcher) Track(tags map[string]Tag) {
	w.mu.Lock()
	defer w.mu.Unlock()

	added := false

	for key, t := range tags {
		cur, ok := w.tags[key]
		if !ok {
			cur = &watchedTag{}
			w.tags[key] = cur
			added = true
		}

		cur.ref = t.Ref
		cur.running = t.Running
	}

	for key := range w.tags {
		if _, ok := tags[key]; !ok {
			delete(w.tags, key)
		}
	}

	if added {
		w.signal()
	}
}

// Tag is the input of Track for one tag.
type Tag struct {
	// Ref is the reference as written on the container.
	Ref string
	// Running are the digests of the containers on the tag; "" stands for a
	// container that is to move to it.
	Running []string
}

// Check returns the last check of a tag.
func (w *watcher) Check(key string) (protocol.RegistryCheck, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	t, ok := w.tags[key]
	if !ok || !t.checked {
		return protocol.RegistryCheck{}, false
	}

	return t.check, true
}

// RecheckAll looks every tag up now.
func (w *watcher) RecheckAll() {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, t := range w.tags {
		t.wantGen++
	}

	w.signal()
}

// Lookup looks the tags up now: the controller handed this host the duty.
func (w *watcher) Lookup(refs []string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, ref := range refs {
		if t, ok := w.tags[imageref.Normalize(ref)]; ok {
			t.wantGen++
		}
	}

	w.signal()
}

// Relay takes in what another host saw a tag point at. A digest that is
// newer than this host's own look is taken, and the local images are looked
// at for it.
func (w *watcher) Relay(digests []protocol.TagDigest) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, d := range digests {
		t, ok := w.tags[imageref.Normalize(d.Ref)]
		if !ok || d.Digest == "" || (t.checked && !d.CheckedAt.After(t.check.CheckedAt)) {
			continue
		}

		if t.check.Digest != d.Digest {
			t.check.Revision, t.check.Pulled = "", false
		}

		t.check.Digest, t.check.CheckedAt, t.check.Error = d.Digest, d.CheckedAt, ""
		t.checked = true
		t.local = true
	}

	w.signal()
}

// SetWindow takes in the pull window: the digests the host may pull, by
// tag. A pull the window no longer names is stopped.
func (w *watcher) SetWindow(window *protocol.PullWindow) {
	w.mu.Lock()
	defer w.mu.Unlock()

	allowed := make(map[string]string, 2)

	if window != nil {
		for _, img := range window.Images {
			allowed[imageref.Normalize(img.Ref)] = img.Digest
		}
	}

	for key, t := range w.tags {
		t.allowed = allowed[key]
	}

	// A pull the window no longer names stops.
	if w.pulling != "" && allowed[w.pulling] == "" && w.cancelPull != nil {
		w.cancelPull()
	}

	w.signal()
}

// SetFallback switches the watcher's own schedule on for every tag (the
// controller has not answered for a while) or off.
func (w *watcher) SetFallback(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.fallback != on {
		w.fallback = on
		w.signal()
	}
}

// Pulling returns the references being pulled now.
func (w *watcher) Pulling() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.tags[w.pulling]; ok && w.pulling != "" {
		return []string{t.ref}
	}

	return nil
}

// Request asks for a fresh lookup of one tag (a roll). It returns a ticket
// for Result.
func (w *watcher) Request(key, ref string) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	t, ok := w.tags[key]
	if !ok {
		t = &watchedTag{ref: ref}
		w.tags[key] = t
	}

	t.wantGen++

	w.signal()

	return t.wantGen
}

// Result returns the check that answers a Request ticket, once a check that
// started after the request has finished. gone is true when the tag is no
// longer followed.
func (w *watcher) Result(key string, ticket uint64) (
	check protocol.RegistryCheck, done, gone bool,
) {
	w.mu.Lock()
	defer w.mu.Unlock()

	t, ok := w.tags[key]
	if !ok {
		return protocol.RegistryCheck{}, false, true
	}

	if t.doneGen < ticket {
		return protocol.RegistryCheck{}, false, false
	}

	return t.check, true, false
}

// signal wakes the run loop; the caller holds w.mu.
func (w *watcher) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run checks due tags, one at a time, until ctx ends.
func (w *watcher) Run(ctx context.Context) {
	for ctx.Err() == nil {
		key, wait := w.nextDue()
		if key != "" {
			w.checkTag(ctx, key)

			continue
		}

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-w.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// own reports whether the tag is looked up on the watcher's own schedule:
// while the controller is not heard, and always for a tag no container of
// the host runs. The caller holds w.mu.
func (w *watcher) own(t *watchedTag) bool {
	return w.fallback || !slices.ContainsFunc(t.running, func(d string) bool { return d != "" })
}

// wantsPull reports whether the window lets the host pull an image it does
// not have. The caller holds w.mu.
func (w *watcher) wantsPull(t *watchedTag, now time.Time) bool {
	return w.pull && t.allowed != "" && t.checked && t.allowed == t.check.Digest && !t.check.Pulled &&
		!now.Before(t.retryAt)
}

// nextDue returns the tag that is due (the longest overdue first), or the
// time to sleep until the next one is.
func (w *watcher) nextDue() (string, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()

	var (
		now  = w.now()
		due  string
		wait = maxWatchIdle
	)

	keys := make([]string, 0, len(w.tags))
	for key := range w.tags {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		t := w.tags[key]

		switch {
		case t.wantGen != t.doneGen, t.local, w.wantsPull(t, now), w.own(t) && !t.next.After(now):
			if due == "" {
				due = key
			}
		case w.own(t):
			wait = min(wait, t.next.Sub(now))
		}
	}

	return due, max(wait, 10*time.Millisecond)
}

// checkTag looks one tag up where that is due, pulls its image when the
// window allows it and records the result.
func (w *watcher) checkTag(ctx context.Context, key string) {
	w.mu.Lock()

	t, ok := w.tags[key]
	if !ok {
		w.mu.Unlock()

		return
	}

	var (
		now      = w.now()
		ref      = t.ref
		startGen = t.wantGen
		lookup   = t.wantGen != t.doneGen || (w.own(t) && !t.next.After(now)) || !t.checked
		allowed  = t.allowed
		result   = t.check
	)

	t.local = false

	w.mu.Unlock()

	log := w.log.WithField("image", ref)
	previous := result.Digest

	if lookup {
		w.lookup(ctx, log, ref, &result)
	}

	if ctx.Err() != nil {
		return
	}

	pulled := false

	if result.Digest != "" {
		pull := w.pull && allowed != "" && allowed == result.Digest
		result.Pulled, pulled = w.ensureLocal(ctx, log, key, ref, &result, pull)
	}

	if ctx.Err() != nil {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	t, ok = w.tags[key]
	if !ok {
		return
	}

	t.check = result
	t.checked = true

	if pulled && !result.Pulled {
		t.retryAt = w.now().Add(pullRetry)
	}

	if lookup {
		t.doneGen = startGen
		t.next = w.now().Add(w.jittered())
	}

	if result.Digest != previous && previous != "" && lookup {
		go w.changed()
	}
}

// lookup asks the registry where the tag points now and records it in
// result; a failure keeps the last good digest.
func (w *watcher) lookup(ctx context.Context, log logrus.FieldLogger, ref string, result *protocol.RegistryCheck) {
	digest, err := w.resolve(ctx, ref)
	if ctx.Err() != nil {
		return
	}

	result.CheckedAt = w.now()
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

// ensureLocal reports whether the image for result.Digest is local, pulling
// it first when pull says so; tried says that a pull was attempted. The
// revision label of the local image and errors are recorded in result.
func (w *watcher) ensureLocal(
	ctx context.Context,
	log logrus.FieldLogger,
	key, ref string,
	result *protocol.RegistryCheck,
	pull bool,
) (local, tried bool) {
	digest, revision, err := w.runtime.LocalImage(ctx, ref)
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

	if !pull {
		return false, false
	}

	log.WithField("digest", result.Digest).Info("pulling image in the pull window")

	pctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	w.mu.Lock()
	w.pulling, w.cancelPull = key, cancel
	w.mu.Unlock()

	got, revision, err := w.runtime.Pull(pctx, ref)

	w.mu.Lock()
	w.pulling, w.cancelPull = "", nil
	w.mu.Unlock()

	if err != nil {
		result.Error = "pull: " + err.Error()

		log.WithError(err).Warn("image pull failed")

		return false, true
	}

	if got != result.Digest {
		// The tag moved between the lookup and the pull; the next check
		// sorts it out.
		result.Error = fmt.Sprintf("pulled digest %s differs from registry digest %s",
			got, result.Digest)

		return false, true
	}

	result.Revision = revision

	log.WithField("digest", got).Info("image pulled")

	return true, true
}

func (w *watcher) resolve(ctx context.Context, ref string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, ResolveTimeout)
	defer cancel()

	digest, err := w.registry.Resolve(rctx, ref)
	if err != nil {
		return "", err
	}

	if digest == "" {
		return "", fmt.Errorf("registry returned no digest for %s", ref)
	}

	return digest, nil
}

// jittered returns the poll interval plus up to pollJitter of it.
func (w *watcher) jittered() time.Duration {
	//nolint:gosec // jitter only spreads load; it needs no cryptographic randomness
	return w.interval + time.Duration(rand.Float64()*pollJitter*float64(w.interval))
}
