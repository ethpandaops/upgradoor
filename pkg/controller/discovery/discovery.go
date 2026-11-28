// Package discovery looks the tags of a devnet's hosts up at the registries.
//
// Every host lists the tags it follows with each sync. The tracker looks
// each tag up once per lookup interval, one at a time, and answers a sync
// with what the host's tags point at. A failed lookup keeps the last good
// digest and is tried again sooner. The hosts never ask a registry: they
// compare the digests with their local images and pull inside a lease.
//
// The tracker keeps what it learned in memory: after a restart it looks the
// tags up again as the hosts list them.
package discovery

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// forgetAfter is how long a tag is followed after the last host listed it.
	forgetAfter = time.Hour
	// lookupTimeout bounds one lookup: the lookups run one at a time, so a
	// registry that does not answer holds the others up for this long.
	lookupTimeout = 20 * time.Second
	// retryAfter is the pause after a failed lookup, when the interval is longer.
	retryAfter = time.Minute
	// maxTags bounds the followed tags of all hosts.
	maxTags = 4096
	// maxRefLength bounds a reference a host lists.
	maxRefLength = 512
	// maxErrorLength bounds a lookup error handed to the hosts.
	maxErrorLength = 300
	// maxIdle bounds how long the goroutine sleeps without looking again.
	maxIdle = time.Minute
	// refreshFloor is the age below which Refresh leaves a lookup alone.
	refreshFloor = 30 * time.Second
	// minIdle keeps a tag that was due in the past from spinning the loop.
	minIdle = 10 * time.Millisecond
)

// Resolver resolves a tag to its digest; registry.Resolver satisfies it.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Tracker follows the tags of the hosts. It is safe for concurrent use.
type Tracker interface {
	// Start runs the lookups until ctx ends or Stop.
	Start(ctx context.Context) error
	// Stop ends the lookups and waits for the one in flight. Safe after a
	// failed Start and more than once.
	Stop() error
	// Sync notes the tags a host follows (at most protocol.MaxTags, each a
	// tag reference) and returns what those looked up so far point at.
	// interval is the lookup interval from here on; zero keeps it.
	Sync(host string, tags []string, interval time.Duration) []protocol.TagDigest
	// Refresh asks for a lookup now of every followed tag whose newest one
	// is older than RefreshFloor, and says how many that are.
	Refresh() Refreshed
	// RefreshTag asks for a lookup now of one followed tag, unless its newest
	// lookup began after since. It reports whether the tag is followed.
	RefreshTag(ref string, since time.Time) bool
	// LookedUp returns the newest lookup of a followed tag; false when the
	// tag is not followed.
	LookedUp(ref string) (Lookup, bool)
	// Tags lists the followed tags with the digest each points at, ordered
	// by reference.
	Tags() []Tag
}

// Tag is a followed tag: the reference as a host wrote it and the digest of
// its newest good lookup, empty before the first.
type Tag struct {
	Ref    string
	Digest string
}

// Lookup is the newest lookup of a tag: the digest of the newest good one,
// and when the newest one began and what failed in it.
type Lookup struct {
	Digest  string
	Started time.Time
	Error   string
}

// RefreshFloor is the age below which Refresh leaves a lookup alone.
const RefreshFloor = refreshFloor

// Refreshed is what Refresh did.
type Refreshed struct {
	// Tags is how many tags are looked up now; Recent how many were looked
	// up within RefreshFloor and are left alone.
	Tags   int
	Recent int
	// Next is when the first of the recent ones may be refreshed again;
	// zero when none is recent.
	Next time.Time
}

// tag is one followed tag.
type tag struct {
	// ref is the reference as the first host wrote it.
	ref string
	// digest is the newest good lookup; startedAt, checkedAt and err are of
	// the newest lookup, good or not.
	digest    string
	startedAt time.Time
	checkedAt time.Time
	err       string
	// hosts are the hosts that follow the tag, with when they last said so.
	hosts map[string]time.Time
	// refreshAt is when a lookup was asked for, whatever the interval says;
	// zero when none is. A lookup that began before it does not answer it.
	refreshAt time.Time
}

type tracker struct {
	log      logrus.FieldLogger
	resolver Resolver
	now      func() time.Time

	// wake is signalled, never blocking, when a tag was added or the
	// interval changed.
	wake chan struct{}

	mu       sync.Mutex
	tags     map[string]*tag // by normalised reference
	interval time.Duration
	// full says the log already tells that maxTags is reached.
	full bool

	lifeMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ Tracker = (*tracker)(nil)

// NewTracker returns a tracker that looks up with resolver; interval is the
// lookup interval until a Sync names one.
func NewTracker(
	log logrus.FieldLogger, resolver Resolver, interval time.Duration, now func() time.Time,
) Tracker {
	return &tracker{
		log:      log.WithField("package", "discovery"),
		resolver: resolver,
		now:      now,
		wake:     make(chan struct{}, 1),
		tags:     make(map[string]*tag, 16),
		interval: interval,
	}
}

// Start implements Tracker.
func (t *tracker) Start(ctx context.Context) error {
	t.lifeMu.Lock()
	defer t.lifeMu.Unlock()

	if t.cancel != nil {
		return errors.New("discovery: tracker already started")
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

// Sync implements Tracker.
func (t *tracker) Sync(host string, tags []string, interval time.Duration) []protocol.TagDigest {
	t.mu.Lock()
	defer t.mu.Unlock()

	changed := interval > 0 && interval != t.interval
	if changed {
		t.interval = interval
	}

	if len(tags) > protocol.MaxTags {
		tags = tags[:protocol.MaxTags]
	}

	var (
		now   = t.now()
		out   = make([]protocol.TagDigest, 0, len(tags))
		added = false
	)

	for _, ref := range tags {
		if len(ref) > maxRefLength {
			continue
		}

		parsed, err := imageref.Parse(ref)
		if err != nil || parsed.Digest != "" {
			continue
		}

		key := parsed.String()

		tg := t.tags[key]
		if tg == nil {
			if len(t.tags) >= maxTags {
				if !t.full {
					t.full = true

					t.log.WithField("host", host).Warn("too many tags: new ones are not looked up")
				}

				continue
			}

			tg = &tag{ref: ref, hosts: make(map[string]time.Time, 4)}
			t.tags[key] = tg
			added = true
		}

		tg.hosts[host] = now

		if !tg.checkedAt.IsZero() {
			out = append(out, protocol.TagDigest{
				Ref: ref, Digest: tg.digest, CheckedAt: tg.checkedAt, Error: tg.err,
			})
		}
	}

	if added || changed {
		t.signal()
	}

	return out
}

// Tags implements Tracker.
func (t *tracker) Tags() []Tag {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]Tag, 0, len(t.tags))
	for _, tg := range t.tags {
		out = append(out, Tag{Ref: tg.ref, Digest: tg.digest})
	}

	slices.SortFunc(out, func(a, b Tag) int { return cmp.Compare(a.Ref, b.Ref) })

	return out
}

// Refresh implements Tracker.
func (t *tracker) Refresh() Refreshed {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()

	var out Refreshed

	for _, tg := range t.tags {
		if !tg.refreshAt.IsZero() || now.Sub(tg.checkedAt) >= refreshFloor {
			tg.ask(now)

			out.Tags++

			continue
		}

		out.Recent++

		if next := tg.checkedAt.Add(refreshFloor); out.Next.IsZero() || next.Before(out.Next) {
			out.Next = next
		}
	}

	if out.Tags > 0 {
		t.signal()
	}

	return out
}

// RefreshTag implements Tracker.
func (t *tracker) RefreshTag(ref string, since time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	tg := t.tag(ref)
	if tg == nil {
		return false
	}

	if !tg.startedAt.After(since) {
		tg.ask(t.now())
		t.signal()
	}

	return true
}

// LookedUp implements Tracker.
func (t *tracker) LookedUp(ref string) (Lookup, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tg := t.tag(ref)
	if tg == nil {
		return Lookup{}, false
	}

	return Lookup{Digest: tg.digest, Started: tg.startedAt, Error: tg.err}, true
}

// tag returns the followed tag of a reference, or nil. The caller holds t.mu.
func (t *tracker) tag(ref string) *tag {
	parsed, err := imageref.Parse(ref)
	if err != nil {
		return nil
	}

	return t.tags[parsed.String()]
}

// ask asks for a lookup of the tag that begins at now or later. The caller
// holds t.mu.
func (tg *tag) ask(now time.Time) {
	tg.refreshAt = later(tg.refreshAt, now)
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

// signal wakes the goroutine without blocking.
func (t *tracker) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// run looks the due tags up, one at a time, until ctx ends.
func (t *tracker) run(ctx context.Context) {
	for ctx.Err() == nil {
		key, wait := t.nextDue()
		if key != "" {
			t.lookup(ctx, key)

			continue
		}

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
		case <-t.wake:
		case <-timer.C:
		}

		timer.Stop()
	}
}

// nextDue forgets the tags no host follows and returns the tag that is due
// (the one that waited longest), or the time to sleep until one is.
func (t *tracker) nextDue() (string, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var (
		now     = t.now()
		due     string
		overdue time.Duration
		wait    = maxIdle
	)

	keys := make([]string, 0, len(t.tags))
	for key := range t.tags {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		tg := t.tags[key]

		for host, seen := range tg.hosts {
			if now.Sub(seen) > forgetAfter {
				delete(tg.hosts, host)
			}
		}

		if len(tg.hosts) == 0 {
			delete(t.tags, key)
			t.full = false

			continue
		}

		// A tag never looked up is due first.
		left := time.Duration(-1 << 62)
		if !tg.checkedAt.IsZero() {
			left = tg.checkedAt.Add(t.pause(tg)).Sub(now)
		}

		if !tg.refreshAt.IsZero() {
			left = min(left, 0)
		}

		if left <= 0 && (due == "" || -left > overdue) {
			due, overdue = key, -left
		}

		if left > 0 {
			wait = min(wait, left)
		}
	}

	return due, max(wait, minIdle)
}

// pause is the time between two lookups of the tag. The caller holds t.mu.
func (t *tracker) pause(tg *tag) time.Duration {
	if tg.err != "" {
		return min(t.interval, retryAfter)
	}

	return t.interval
}

// lookup asks the registry where one tag points, without the lock held.
func (t *tracker) lookup(ctx context.Context, key string) {
	t.mu.Lock()

	tg, ok := t.tags[key]
	if !ok {
		t.mu.Unlock()

		return
	}

	ref, started := tg.ref, t.now()

	t.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	digest, err := t.resolver.Resolve(rctx, ref)

	cancel()

	if ctx.Err() != nil {
		return
	}

	if err == nil && digest == "" {
		err = errors.New("registry returned no digest")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.tags[key] != tg {
		return
	}

	log := t.log.WithField("image", ref)
	tg.startedAt, tg.checkedAt = started, t.now()

	if !tg.refreshAt.After(started) {
		tg.refreshAt = time.Time{}
	}

	if err != nil {
		msg := err.Error()
		if len(msg) > maxErrorLength {
			msg = msg[:maxErrorLength]
		}

		// Logged once per failure, not once per retry.
		if msg != tg.err {
			log.WithError(err).Warn("registry lookup failed")
		}

		tg.err = msg

		return
	}

	if digest != tg.digest {
		log.WithFields(logrus.Fields{"digest": digest, "previous": tg.digest}).
			Info("tag points at a new digest")
	}

	tg.digest, tg.err = digest, ""
}
