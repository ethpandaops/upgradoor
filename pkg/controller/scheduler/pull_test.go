package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// wantsUnpulled is wantsImage for a host that does not have the image yet.
func (b *worldBuilder) wantsUnpulled(host, image, to string) {
	b.wantsImage(host, image, to)

	h := b.get(host)
	n := &h.Status.Needs[len(h.Status.Needs)-1]
	n.Unpulled = []protocol.PullImage{{Ref: image, Digest: to}}
	h.Status.Containers[len(h.Status.Containers)-1].Registry.Pulled = false
}

// pulled makes the host report its images local.
func (b *worldBuilder) pulled(host string) {
	h := b.get(host)
	for i := range h.Status.Needs {
		h.Status.Needs[i].Unpulled = nil
	}

	for i := range h.Status.Containers {
		if rc := h.Status.Containers[i].Registry; rc != nil {
			rc.Pulled = true
		}
	}
}

func (b *worldBuilder) windows() []string {
	out := make([]string, 0, len(b.w.Hosts))

	for _, h := range b.w.Hosts {
		if h.Pull.Open() {
			out = append(out, h.Name)
		}
	}

	return out
}

// At most maxPullingPercent of the hosts pull at once, nearest to their slot
// first; a need that waits for its image is not granted.
func TestPullWindows(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.MaxPullingPercent = 50

	for _, h := range []string{"a", "b", "c", "d"} {
		b.host(h, 10)
		b.wantsUnpulled(h, imgLighthouse, digestA)
	}

	out := b.step()

	assert.Empty(t, b.w.Leases, "nothing is granted before the images are local")
	assert.Len(t, b.windows(), 2, "half of the four hosts pull")
	assert.Equal(t, PullPulling, out.Plan.Hosts[b.windows()[0]].Pull)
	assert.Equal(t, reasonPull, queueReasons(out.Plan)["a"])

	waiting := 0

	for _, hp := range out.Plan.Hosts {
		if hp.Pull == PullWaiting {
			waiting++
		}
	}

	assert.Equal(t, 2, waiting)

	// The first two pulled: their windows end, but count until their agents
	// have synced after the end without pulling.
	first := b.windows()
	for _, h := range first {
		b.pulled(h)
	}

	b.advance(time.Second)
	b.step()

	assert.Empty(t, b.windows(), "ended windows are not offered")
	require.NotNil(t, b.get(first[0]).Pull, "but they count")

	for _, h := range first {
		b.get(h).LastSeen = b.w.Now.Add(time.Second)
	}

	b.advance(2 * time.Second)
	b.step()

	assert.Len(t, b.windows(), 2, "the other two pull now")
	assert.NotNil(t, b.lease(first[0]), "a canary with its image local is granted")
}

// A rollout that waits for approval gets no pull window.
func TestNoPullBeforeApproval(t *testing.T) {
	b := newWorldBuilder(t)
	b.w.Settings.PolicyImage = fleet.PolicyManual
	b.host("a", 10)
	b.wantsUnpulled("a", imgLighthouse, digestA)

	out := b.step()

	assert.Empty(t, b.windows())
	assert.Equal(t, "", out.Plan.Hosts["a"].Pull, "no pull state: the host does not wait to be served")
}
