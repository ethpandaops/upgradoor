package agent

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// resolveTimeout bounds one registry lookup.
	resolveTimeout = time.Minute
	// pullTimeout bounds one image pull.
	pullTimeout = 30 * time.Minute
	// pollJitter is the share of the poll interval added at random.
	pollJitter = 0.2
	// maxWatchIdle bounds how long the watcher sleeps without looking again.
	maxWatchIdle = time.Minute
)

// imageWatcher follows the tags of the managed containers: it asks the
// registry where each tag points and pulls a new image as soon as the tag
// has moved, so that the image is local before a slot is requested.
//
// It runs in its own goroutine because a pull can take minutes and the sync
// loop must not wait for it. Pulling never touches a running container.
type imageWatcher struct {
	log      logrus.FieldLogger
	runtime  hostapi.ContainerRuntime
	registry hostapi.Registry
	interval time.Duration
	now      func() time.Time
	// pull is false on a host that never rolls: tags are resolved for the
	// report, images are not pulled.
	pull bool

	mu   sync.Mutex
	tags map[string]*watchedTag // by normalised reference
	wake chan struct{}
}

// watchedTag is one followed tag.
type watchedTag struct {
	// ref is the reference as written on the container.
	ref string
	// running are the digests of the containers on this tag.
	running []string
	check   protocol.RegistryCheck
	checked bool
	// next is when the tag is due; zero means now.
	next time.Time
	// wantGen counts explicit check requests; doneGen is the request count
	// the last finished check had seen when it started.
	wantGen, doneGen uint64
	// forcePull pulls even when every container already runs the registry
	// digest (manual roll).
	forcePull bool
}

func newImageWatcher(
	log logrus.FieldLogger,
	runtime hostapi.ContainerRuntime,
	registry hostapi.Registry,
	interval time.Duration,
	now func() time.Time,
	pull bool,
) *imageWatcher {
	return &imageWatcher{
		log:      log.WithField("component", "imagewatch"),
		runtime:  runtime,
		registry: registry,
		interval: interval,
		now:      now,
		pull:     pull,
		tags:     make(map[string]*watchedTag, 8),
		wake:     make(chan struct{}, 1),
	}
}

// Track sets the tags to follow: reference as written, by normalised
// reference, with the digests the containers on it run. Tags that are no
// longer listed are forgotten.
func (w *imageWatcher) Track(tags map[string]trackedTag) {
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

		cur.ref = t.ref
		cur.running = t.running
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

// trackedTag is the input of Track for one tag.
type trackedTag struct {
	ref     string
	running []string
}

// Check returns the last check of a tag.
func (w *imageWatcher) Check(key string) (protocol.RegistryCheck, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	t, ok := w.tags[key]
	if !ok || !t.checked {
		return protocol.RegistryCheck{}, false
	}

	return t.check, true
}

// RecheckAll makes every tag due now.
func (w *imageWatcher) RecheckAll() {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, t := range w.tags {
		t.next = time.Time{}
		t.wantGen++
	}

	w.signal()
}

// Request asks for a fresh check of one tag, pulling the image even if no
// container is behind. It returns a ticket for Result.
func (w *imageWatcher) Request(key, ref string) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	t, ok := w.tags[key]
	if !ok {
		t = &watchedTag{ref: ref}
		w.tags[key] = t
	}

	t.next = time.Time{}
	t.wantGen++
	t.forcePull = true

	w.signal()

	return t.wantGen
}

// Result returns the check that answers a Request ticket, once a check that
// started after the request has finished. gone is true when the tag is no
// longer followed.
func (w *imageWatcher) Result(key string, ticket uint64) (
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
func (w *imageWatcher) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// run checks due tags, one at a time, until ctx ends.
func (w *imageWatcher) run(ctx context.Context) {
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

// nextDue returns the tag that is due (the longest overdue first), or the
// time to sleep until the next one is.
func (w *imageWatcher) nextDue() (string, time.Duration) {
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

		if !t.next.After(now) {
			if due == "" || t.next.Before(w.tags[due].next) {
				due = key
			}

			continue
		}

		wait = min(wait, t.next.Sub(now))
	}

	return due, max(wait, 10*time.Millisecond)
}

// checkTag resolves one tag, pulls its image when a container is behind
// (and this host rolls at all) and records the result.
func (w *imageWatcher) checkTag(ctx context.Context, key string) {
	w.mu.Lock()

	t, ok := w.tags[key]
	if !ok {
		w.mu.Unlock()

		return
	}

	var (
		ref      = t.ref
		running  = slices.Clone(t.running)
		force    = t.forcePull
		startGen = t.wantGen
		result   = t.check
	)

	w.mu.Unlock()

	log := w.log.WithField("image", ref)

	digest, err := w.resolve(ctx, ref)
	if ctx.Err() != nil {
		return
	}

	result.CheckedAt = w.now()
	result.Error = ""

	switch {
	case err != nil:
		// Keep the last good digest.
		result.Error = "registry: " + err.Error()

		log.WithError(err).Warn("registry check failed")
	case digest != result.Digest:
		log.WithFields(logrus.Fields{"digest": digest, "previous": result.Digest}).
			Info("tag points at a new digest")

		// The revision of the new image is known once the image is local.
		result.Digest, result.Revision = digest, ""
	}

	if result.Digest != "" {
		pull := w.pull && err == nil && (force || behind(running, result.Digest))
		result.Pulled = w.ensureLocal(ctx, log, ref, &result, pull)
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
	t.doneGen = startGen

	if t.wantGen != startGen {
		// A request arrived while this check ran: look again right away.
		t.next = time.Time{}

		return
	}

	t.forcePull = false
	t.next = w.now().Add(w.jittered())
}

// ensureLocal reports whether the image for result.Digest is local, pulling
// it first when wanted. The revision label of the local image and errors
// are recorded in result.
func (w *imageWatcher) ensureLocal(
	ctx context.Context,
	log logrus.FieldLogger,
	ref string,
	result *protocol.RegistryCheck,
	pull bool,
) bool {
	local, revision, err := w.runtime.LocalImage(ctx, ref)
	if err != nil {
		if result.Error == "" {
			result.Error = "local image: " + err.Error()
		}

		return false
	}

	if local == result.Digest {
		result.Revision = revision

		return true
	}

	if !pull {
		return false
	}

	log.WithField("digest", result.Digest).Info("pulling image")

	pctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	got, revision, err := w.runtime.Pull(pctx, ref)
	if err != nil {
		result.Error = "pull: " + err.Error()

		log.WithError(err).Warn("image pull failed")

		return false
	}

	if got != result.Digest {
		// The tag moved between the lookup and the pull; the next check
		// sorts it out.
		result.Error = fmt.Sprintf("pulled digest %s differs from registry digest %s",
			got, result.Digest)

		return false
	}

	result.Revision = revision

	log.WithField("digest", got).Info("image pulled")

	return true
}

func (w *imageWatcher) resolve(ctx context.Context, ref string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
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
func (w *imageWatcher) jittered() time.Duration {
	//nolint:gosec // jitter only spreads load; it needs no cryptographic randomness
	return w.interval + time.Duration(rand.Float64()*pollJitter*float64(w.interval))
}

// behind reports whether any container runs another digest than want.
func behind(running []string, want string) bool {
	for _, d := range running {
		if d != want {
			return true
		}
	}

	return false
}
