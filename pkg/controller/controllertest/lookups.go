package controllertest

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Lookups is a discovery.Tracker that answers from Digests (by reference as
// the host writes it) and keeps the last tags and interval of every host.
type Lookups struct {
	mu       sync.Mutex
	Digests  map[string]protocol.TagDigest
	Listed   map[string][]string
	Interval time.Duration
	// Refreshed counts the calls of Refresh, which answers Refreshable.
	Refreshed   int
	Refreshable discovery.Refreshed
	// RefreshedTags lists the references of the calls of RefreshTag.
	RefreshedTags []string
}

// Start does nothing.
func (f *Lookups) Start(context.Context) error { return nil }

// Stop does nothing.
func (f *Lookups) Stop() error { return nil }

// Sync records the host's tags and answers those in Digests.
func (f *Lookups) Sync(host string, tags []string, interval time.Duration) []protocol.TagDigest {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.Listed == nil {
		f.Listed = make(map[string][]string, 2)
	}

	f.Listed[host], f.Interval = slices.Clone(tags), interval

	var out []protocol.TagDigest

	for _, ref := range tags {
		if d, ok := f.Digests[ref]; ok {
			out = append(out, d)
		}
	}

	return out
}

// Refresh counts the call.
func (f *Lookups) Refresh() discovery.Refreshed {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Refreshed++

	return f.Refreshable
}

// RefreshTag records the call; a tag is followed when Digests has it.
func (f *Lookups) RefreshTag(ref string, _ time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.RefreshedTags = append(f.RefreshedTags, ref)
	_, ok := f.Digests[ref]

	return ok
}

// LookedUp answers from Digests, with CheckedAt as the start of the lookup.
func (f *Lookups) LookedUp(ref string) (discovery.Lookup, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	d, ok := f.Digests[ref]

	return discovery.Lookup{Digest: d.Digest, Started: d.CheckedAt, Error: d.Error}, ok
}

// Tags lists Digests.
func (f *Lookups) Tags() []discovery.Tag {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]discovery.Tag, 0, len(f.Digests))
	for ref, d := range f.Digests {
		out = append(out, discovery.Tag{Ref: ref, Digest: d.Digest})
	}

	slices.SortFunc(out, func(a, b discovery.Tag) int { return cmp.Compare(a.Ref, b.Ref) })

	return out
}
