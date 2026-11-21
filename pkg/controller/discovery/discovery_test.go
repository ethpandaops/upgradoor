package discovery

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const image = "ethpandaops/geth:master"

func syncOf(digest string, checked time.Time) *protocol.SyncRequest {
	c := protocol.Container{Name: "geth", Image: image, Managed: true, Digest: "sha256:old"}
	if digest != "" {
		c.Registry = &protocol.RegistryCheck{Digest: digest, CheckedAt: checked}
	}

	return &protocol.SyncRequest{Containers: []protocol.Container{c, {Name: "debug", Image: "busybox:1"}}}
}

// One host at a time looks a tag up, in turn; what it saw is relayed to the
// others.
func TestOneHostLooksUpAndTheOthersAreTold(t *testing.T) {
	tr := NewTracker()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	interval := 5 * time.Minute

	lookups, digests := tr.Sync("a", syncOf("", time.Time{}), now, interval)
	assert.Equal(t, []string{image}, lookups, "nobody looked yet: a gets the duty")
	assert.Empty(t, digests)

	lookups, _ = tr.Sync("b", syncOf("", time.Time{}), now.Add(time.Second), interval)
	assert.Empty(t, lookups, "a holds the duty")

	// a reports a new digest; b hears of it, without a lookup of its own.
	tr.Sync("a", syncOf("sha256:new", now.Add(10*time.Second)), now.Add(20*time.Second), interval)

	lookups, digests = tr.Sync("b", syncOf("", time.Time{}), now.Add(30*time.Second), interval)
	assert.Empty(t, lookups)
	assert.Equal(t, []protocol.TagDigest{{Ref: image, Digest: "sha256:new", CheckedAt: now.Add(10 * time.Second)}}, digests)

	_, digests = tr.Sync("b", syncOf("sha256:new", now.Add(10*time.Second)), now.Add(time.Minute), interval)
	assert.Empty(t, digests, "b knows")

	// An interval later it is the other host's turn, not a's again.
	later := now.Add(10*time.Second + interval + time.Second)

	lookups, _ = tr.Sync("b", syncOf("sha256:new", now.Add(10*time.Second)), later.Add(-10*time.Second), interval)
	assert.Empty(t, lookups, "not due yet")

	lookups, _ = tr.Sync("a", syncOf("sha256:new", now.Add(10*time.Second)), later, interval)
	assert.Empty(t, lookups, "a looked last; b is around")

	lookups, _ = tr.Sync("b", syncOf("sha256:new", now.Add(10*time.Second)), later.Add(time.Second), interval)
	assert.Equal(t, []string{image}, lookups)

	// A host that does not answer its duty in time loses it.
	lookups, _ = tr.Sync("a", syncOf("sha256:new", now.Add(10*time.Second)), later.Add(3*time.Minute), interval)
	assert.Equal(t, []string{image}, lookups, "b let the duty lapse")
}

func TestForget(t *testing.T) {
	tr := NewTracker()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	lookups, _ := tr.Sync("a", syncOf("", time.Time{}), now, time.Minute)
	assert.NotEmpty(t, lookups)

	tr.Forget("a")

	lookups, _ = tr.Sync("b", syncOf("", time.Time{}), now.Add(time.Second), time.Minute)
	assert.Equal(t, []string{image}, lookups, "the forgotten host's duty is free")
}
