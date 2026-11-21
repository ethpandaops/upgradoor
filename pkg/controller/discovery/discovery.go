// Package discovery coordinates the registry lookups of a devnet's agents.
//
// Every tag that managed containers run is looked up by one host at a time:
// each lookup interval the controller hands the duty for the tag to one of
// the hosts that run it, in turn, in that host's sync answer. A host that
// sees a new digest reports it with its next sync; the controller relays the
// digest to every other host on the tag in their sync answers. The relay is a
// signal only: each agent looks at its own images and pulls in a pull window.
// The controller never contacts a registry.
//
// The tracker keeps what it learned in memory: after a restart the hosts'
// reports fill it again within one sync interval.
package discovery

import (
	"slices"
	"sync"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// forgetAfter is how long a host that has not synced still counts as
	// running a tag.
	forgetAfter = time.Hour
	// minDutyTimeout bounds how long a duty is waited for before another
	// host gets it.
	minDutyTimeout = 2 * time.Minute
)

// Tracker is the lookup state of the devnet's tags. It is safe for
// concurrent use.
type Tracker interface {
	// Sync takes in what a host reports of its tags and returns the tags the
	// host is to look up now and the digests to relay to it.
	Sync(host string, req *protocol.SyncRequest, now time.Time, interval time.Duration) (
		lookups []string, digests []protocol.TagDigest)
	// Forget drops a host.
	Forget(host string)
}

// tag is one followed tag.
type tag struct {
	// digest and checkedAt are the newest good lookup of any host; by is
	// that host.
	digest    string
	checkedAt time.Time
	by        string
	// duty is the host that was handed the lookup at dutyAt, until its
	// report of a lookup after that arrives.
	duty   string
	dutyAt time.Time
	// hosts are the hosts that run the tag, with when they last said so.
	hosts map[string]time.Time
}

type tracker struct {
	mu   sync.Mutex
	tags map[string]*tag
}

// NewTracker returns an empty tracker.
func NewTracker() Tracker {
	return &tracker{tags: make(map[string]*tag, 16)}
}

// followed returns the tags the host's managed containers run, by
// normalized reference, with the reference as the host writes it and its
// own last check.
func followed(req *protocol.SyncRequest) map[string]protocol.Container {
	out := make(map[string]protocol.Container, len(req.Containers))

	for i := range req.Containers {
		c := req.Containers[i]
		if !c.Managed || !imageref.IsTag(c.Image) {
			continue
		}

		key := imageref.Normalize(c.Image)
		if cur, ok := out[key]; ok && cur.Registry != nil {
			continue
		}

		out[key] = c
	}

	return out
}

func (t *tracker) Sync(host string, req *protocol.SyncRequest, now time.Time, interval time.Duration) (
	lookups []string, digests []protocol.TagDigest,
) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tags := followed(req)
	dutyTimeout := max(minDutyTimeout, interval/2)

	for _, key := range sortedKeys(tags) {
		c := tags[key]

		tg := t.tags[key]
		if tg == nil {
			tg = &tag{hosts: make(map[string]time.Time, 4)}
			t.tags[key] = tg
		}

		tg.hosts[host] = now

		if rc := c.Registry; rc != nil && rc.Error == "" && rc.Digest != "" && rc.CheckedAt.After(tg.checkedAt) &&
			!rc.CheckedAt.After(now.Add(time.Minute)) {
			tg.digest, tg.checkedAt, tg.by = rc.Digest, rc.CheckedAt, host
		}

		if tg.duty == host && c.Registry != nil && !c.Registry.CheckedAt.Before(tg.dutyAt) {
			tg.duty = ""
		}

		// Relay what another host saw.
		if tg.digest != "" && (c.Registry == nil || c.Registry.Digest != tg.digest) {
			digests = append(digests, protocol.TagDigest{Ref: c.Image, Digest: tg.digest, CheckedAt: tg.checkedAt})
		}

		if t.due(tg, host, now, interval, dutyTimeout) {
			tg.duty, tg.dutyAt = host, now

			lookups = append(lookups, c.Image)
		}
	}

	t.prune(now)

	return lookups, digests
}

// due reports whether the host is to look the tag up now: the newest lookup
// is an interval old, nobody holds the duty (or the one who does let it
// lapse), and it is the host's turn: the host that looked last goes after
// the others.
func (t *tracker) due(tg *tag, host string, now time.Time, interval, dutyTimeout time.Duration) bool {
	if now.Sub(tg.checkedAt) < interval {
		return false
	}

	if tg.duty != "" && now.Sub(tg.dutyAt) < dutyTimeout {
		return false
	}

	if host != tg.by || len(tg.hosts) == 1 {
		return true
	}

	// The host looked last: another one that is around goes first, unless
	// the tag waited a duty timeout longer for one.
	for other, seen := range tg.hosts {
		if other != host && now.Sub(seen) < dutyTimeout {
			return now.Sub(tg.checkedAt) >= interval+dutyTimeout
		}
	}

	return true
}

// prune forgets hosts that have not synced for long and tags nobody runs.
func (t *tracker) prune(now time.Time) {
	for key, tg := range t.tags {
		for host, seen := range tg.hosts {
			if now.Sub(seen) > forgetAfter {
				delete(tg.hosts, host)
			}
		}

		if len(tg.hosts) == 0 {
			delete(t.tags, key)
		}
	}
}

func (t *tracker) Forget(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, tg := range t.tags {
		delete(tg.hosts, host)

		if tg.duty == host {
			tg.duty = ""
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
