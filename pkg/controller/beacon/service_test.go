package beacon

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

const (
	pathGenesis  = "/eth/v1/beacon/genesis"
	pathSpec     = "/eth/v1/config/spec"
	pathHead     = "/eth/v1/beacon/headers/head"
	pathFinality = "/eth/v1/beacon/states/head/finality_checkpoints"
)

func TestPollPublishesChainView(t *testing.T) {
	h := newHarness(t, 1, newFakeStore(), nil)

	assert.Equal(t, fleet.NetworkHealth{}, h.svc.Health(), "empty before the first poll")

	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.Equal(t, fleet.NetworkHealth{
		UpdatedAt:         at(10, 30*time.Second),
		GenesisTime:       testGenesis,
		SecondsPerSlot:    12,
		SlotsPerEpoch:     8,
		HeadSlot:          82,
		CurrentEpoch:      10,
		FinalizedEpoch:    8,
		FinalityLagEpochs: 2,
	}, got)
	assert.Equal(t, testEpoch, got.EpochDuration())

	// Finality stalls while the clock moves on: the lag grows.
	h.clock.Set(at(14, time.Second))
	h.node.set(func(n *fakeNode) { n.headSlot = 112 })
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.Equal(t, uint64(14), got.CurrentEpoch)
	assert.Equal(t, uint64(112), got.HeadSlot)
	assert.Equal(t, uint64(6), got.FinalityLagEpochs)
	assert.Equal(t, at(14, time.Second), got.UpdatedAt)

	// Genesis and spec are read once.
	assert.Equal(t, 1, h.node.count(pathGenesis))
	assert.Equal(t, 1, h.node.count(pathSpec))
	assert.Equal(t, 2, h.node.count(pathHead))
	assert.Equal(t, 2, h.node.count(pathFinality))
}

func TestPollTimingFromSpec(t *testing.T) {
	tests := []struct {
		name        string
		spec        map[string]any
		now         time.Time
		wantSeconds uint64
		wantEpoch   uint64
	}{
		{
			name:        "seconds per slot",
			spec:        map[string]any{"SECONDS_PER_SLOT": "12", "SLOTS_PER_EPOCH": "8"},
			now:         at(10, 30*time.Second),
			wantSeconds: 12,
			wantEpoch:   10,
		},
		{
			name: "slot duration in milliseconds",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOT_DURATION_MS": "6000", "SLOTS_PER_EPOCH": "8",
			},
			now:         at(10, 30*time.Second),
			wantSeconds: 6,
			wantEpoch:   20,
		},
		{
			name: "slot duration changed by a fork",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOT_DURATION_MS": "12000", "SLOTS_PER_EPOCH": "8",
				"SLOT_DURATION_MS_HEZE": "6000", "HEZE_FORK_EPOCH": "4",
			},
			// 4 epochs of 96 s, then 48 s epochs: 6 old epochs later is 12 new ones.
			now:         at(10, 30*time.Second),
			wantSeconds: 6,
			wantEpoch:   16,
		},
		{
			name: "before the fork the old duration is reported",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOT_DURATION_MS": "12000", "SLOTS_PER_EPOCH": "8",
				"SLOT_DURATION_MS_HEZE": "6000", "HEZE_FORK_EPOCH": "40",
			},
			now:         at(10, 30*time.Second),
			wantSeconds: 12,
			wantEpoch:   10,
		},
		{
			name:        "before genesis",
			spec:        map[string]any{"SECONDS_PER_SLOT": "12", "SLOTS_PER_EPOCH": "8"},
			now:         testGenesis.Add(-time.Hour),
			wantSeconds: 12,
			wantEpoch:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, 1, newFakeStore(host("a", 0, 4)), nil)
			h.node.set(func(n *fakeNode) {
				n.spec = tt.spec
				n.headSlot = 0
				n.finalized = 0
			})
			h.clock.Set(tt.now)

			h.svc.poll(t.Context())

			got := h.svc.Health()
			assert.Empty(t, got.Error)
			assert.Equal(t, tt.wantSeconds, got.SecondsPerSlot)
			assert.Equal(t, uint64(8), got.SlotsPerEpoch)
			assert.Equal(t, tt.wantEpoch, got.CurrentEpoch)
			assert.Equal(t, tt.wantEpoch, got.FinalityLagEpochs)

			if tt.wantEpoch == 0 {
				assert.Empty(t, h.node.livenessCalls(), "no complete epoch yet")
			}
		})
	}
}

func TestPollRetriesTimingUntilAvailable(t *testing.T) {
	h := newHarness(t, 1, newFakeStore(), nil)
	h.node.set(func(n *fakeNode) { n.down = true })

	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.True(t, got.UpdatedAt.IsZero())
	assert.True(t, got.GenesisTime.IsZero())
	assert.Contains(t, got.Error, "reading genesis")
	assert.Contains(t, got.Error, "HTTP 503: beacon node is syncing")

	// Genesis answers, the spec does not: still not ready.
	h.node.set(func(n *fakeNode) {
		n.down = false
		n.failPath = pathSpec
	})
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.True(t, got.UpdatedAt.IsZero())
	assert.Contains(t, got.Error, "reading chain spec")

	h.node.set(func(n *fakeNode) { n.failPath = "" })
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.Empty(t, got.Error)
	assert.Equal(t, h.clock.Now(), got.UpdatedAt)
	assert.Equal(t, testGenesis, got.GenesisTime)
}

func TestPollUnusableSpec(t *testing.T) {
	h := newHarness(t, 1, newFakeStore(), nil)
	h.node.set(func(n *fakeNode) { n.spec = map[string]any{"SECONDS_PER_SLOT": "12"} })

	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.Contains(t, got.Error, "SLOTS_PER_EPOCH")
	assert.True(t, got.UpdatedAt.IsZero())
}

func TestPollFailureKeepsLastView(t *testing.T) {
	h := newHarness(t, 1, newFakeStore(host("a", 0, 4)), nil)
	h.svc.poll(t.Context())

	first := h.svc.Health()
	require.Empty(t, first.Error)
	require.True(t, first.LivenessKnown)

	for _, path := range []string{pathHead, pathFinality} {
		h.clock.Advance(testSlot)
		h.node.set(func(n *fakeNode) {
			n.failPath = path
			n.headSlot = 999
		})
		h.svc.poll(t.Context())

		got := h.svc.Health()
		assert.Contains(t, got.Error, "HTTP 500: internal error")
		assert.Equal(t, first.UpdatedAt, got.UpdatedAt, "UpdatedAt moves on success only")
		assert.Equal(t, first.HeadSlot, got.HeadSlot)
		assert.Equal(t, first.FinalizedEpoch, got.FinalizedEpoch)
		assert.True(t, got.LivenessKnown)
		assert.Equal(t, first.Tracked, got.Tracked)
	}

	h.node.set(func(n *fakeNode) {
		n.failPath = ""
		n.headSlot = 85
	})
	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.Empty(t, got.Error)
	assert.Equal(t, h.clock.Now(), got.UpdatedAt)
	assert.Equal(t, uint64(85), got.HeadSlot)
}

func TestRestartWithNodeDownShowsStoredLiveness(t *testing.T) {
	store := newFakeStore(host("a", 0, 4), host("b", 4, 10))
	h := newHarness(t, 1, store, nil)
	h.node.set(func(n *fakeNode) {
		n.isLive = func(_, index uint64) bool { return index != 0 }
	})
	h.svc.poll(t.Context())
	require.Equal(t, uint64(9), h.svc.Health().LivenessEpoch)

	// The controller restarts while the node is unreachable: not even the
	// chain timing can be loaded.
	h.node.set(func(n *fakeNode) { n.down = true })
	h.clock.Advance(testSlot)

	restarted := h.build(t)
	restarted.poll(t.Context())

	got := restarted.Health()
	assert.Contains(t, got.Error, "reading genesis")
	assert.True(t, got.UpdatedAt.IsZero(), "the chain view never succeeded")
	assert.True(t, got.LivenessKnown, "the stored rows are published without the node")
	assert.Equal(t, uint64(9), got.LivenessEpoch)
	assert.Equal(t, uint64(10), got.Tracked)
	assert.Equal(t, uint64(9), got.Live)

	// A store failure on top does not hide the chain error or the summary.
	store.set(func(s *fakeStore) { s.listErr = errStore })
	restarted.poll(t.Context())

	got = restarted.Health()
	assert.Contains(t, got.Error, "reading genesis")
	assert.True(t, got.LivenessKnown)
	assert.Equal(t, uint64(10), got.Tracked)

	// Head fails with the timing known: same thing.
	store.set(func(s *fakeStore) { s.listErr = nil })
	h.node.set(func(n *fakeNode) {
		n.down = false
		n.failPath = pathHead
	})

	second := h.build(t)
	second.poll(t.Context())

	got = second.Health()
	assert.Contains(t, got.Error, "reading head")
	assert.True(t, got.UpdatedAt.IsZero())
	assert.Equal(t, uint64(12), got.SecondsPerSlot)
	assert.True(t, got.LivenessKnown)
	assert.Equal(t, uint64(9), got.LivenessEpoch)
	assert.Equal(t, uint64(9), got.Live)

	// A fresh store has nothing to publish.
	empty := newHarness(t, 1, newFakeStore(host("a", 0, 4)), nil)
	empty.node.set(func(n *fakeNode) { n.down = true })
	empty.svc.poll(t.Context())

	got = empty.svc.Health()
	assert.NotEmpty(t, got.Error)
	assert.False(t, got.LivenessKnown)
	assert.Zero(t, got.Tracked)
}

func TestSlotDurationNotWholeSeconds(t *testing.T) {
	h := newHarness(t, 1, newFakeStore(host("a", 0, 4)), nil)
	h.node.set(func(n *fakeNode) {
		n.spec = map[string]any{
			"SECONDS_PER_SLOT": "12", "SLOT_DURATION_MS": "8500", "SLOTS_PER_EPOCH": "8",
		}
	})

	const warning = "not a whole number of seconds"

	h.svc.poll(t.Context())
	h.clock.Advance(testSlot)
	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.Empty(t, got.Error)
	assert.Equal(t, uint64(9), got.SecondsPerSlot, "the contract carries whole seconds")
	// 1002 s into the chain with 68 s epochs: the service itself is exact
	// (72 s epochs from the rounded value would say 13).
	assert.Equal(t, uint64(14), got.CurrentEpoch)
	assert.Equal(t, uint64(13), got.LivenessEpoch)
	assert.Equal(t, 1, h.warnings(warning), "the rounding is flagged once")

	// Whole seconds are not flagged.
	whole := newHarness(t, 1, newFakeStore(host("a", 0, 4)), nil)
	whole.svc.poll(t.Context())

	assert.Zero(t, whole.warnings(warning))
}

func TestLivenessPerHost(t *testing.T) {
	store := newFakeStore(
		host("validator-a", 0, 4),
		host("bootnode", 0, 0),
		host("validator-b", 4, 10),
		host("validator-c", 100, 103),
		nil,
	)
	h := newHarness(t, 1, store, nil)
	h.node.set(func(n *fakeNode) {
		n.minEpoch = 9
		// Even indices attest; validator-c is down.
		n.isLive = func(_, index uint64) bool { return index%2 == 0 && index < 100 }
	})

	h.svc.poll(t.Context())

	wantRows := map[string]fleet.Liveness{
		"validator-a": {Host: "validator-a", Epoch: 9, Live: 2, Total: 4},
		"validator-b": {Host: "validator-b", Epoch: 9, Live: 3, Total: 6},
		"validator-c": {Host: "validator-c", Epoch: 9, Live: 0, Total: 3},
	}
	for name, want := range wantRows {
		got, ok := store.row(name, 9)
		require.True(t, ok, name)
		assert.Equal(t, want, got)
	}

	_, ok := store.row("bootnode", 9)
	assert.False(t, ok, "hosts without validators get no row")
	assert.Equal(t, []uint64{9}, store.epochs())

	calls := h.node.livenessCalls()
	require.NotEmpty(t, calls)
	assert.Equal(t, uint64(9), calls[0].epoch)
	assert.Equal(t, []uint64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 100, 101, 102}, calls[0].indices)

	got := h.svc.Health()
	assert.Empty(t, got.Error)
	assert.True(t, got.LivenessKnown)
	assert.Equal(t, uint64(9), got.LivenessEpoch)
	assert.Equal(t, uint64(13), got.Tracked)
	assert.Equal(t, uint64(5), got.Live)
	assert.InDelta(t, 5.0/13.0, got.Participation(), 1e-9)
}

func TestLivenessMissingIndicesAreNotLive(t *testing.T) {
	// A node that leaves indices out of its answer must not make them live.
	store := newFakeStore(host("a", 0, 4))
	h := newHarness(t, 1, store, nil)

	h.svc.timing = nil
	require.NoError(t, h.svc.loadTiming(t.Context()))

	live, err := h.svc.queryLiveness(t.Context(), 9, []uint64{0, 1})
	require.NoError(t, err)
	assert.Len(t, live, 2)

	h.node.set(func(n *fakeNode) { n.isLive = func(_, index uint64) bool { return index == 1 } })

	live, err = h.svc.queryLiveness(t.Context(), 9, []uint64{0, 1, 2})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]struct{}{1: {}}, live)
}

func TestLivenessChunking(t *testing.T) {
	store := newFakeStore(host("a", 0, 6), host("b", 6, 10))
	h := newHarness(t, 1, store, func(o *Options) { o.LivenessChunkSize = 4 })
	h.node.set(func(n *fakeNode) {
		n.minEpoch = 9
		n.isLive = func(_, index uint64) bool { return index != 5 }
	})

	h.svc.poll(t.Context())

	calls := h.node.livenessCalls()
	require.GreaterOrEqual(t, len(calls), 3)

	sizes := make([]int, 0, 3)
	seen := make(map[uint64]int, 10)

	for _, call := range calls[:3] {
		require.Equal(t, uint64(9), call.epoch)

		sizes = append(sizes, len(call.indices))

		for _, index := range call.indices {
			seen[index]++
		}
	}

	assert.Equal(t, []int{4, 4, 2}, sizes)
	assert.Len(t, seen, 10)

	for index, count := range seen {
		assert.Equal(t, 1, count, "index %d", index)
	}

	a, _ := store.row("a", 9)
	b, _ := store.row("b", 9)
	assert.Equal(t, fleet.Liveness{Host: "a", Epoch: 9, Live: 5, Total: 6}, a)
	assert.Equal(t, fleet.Liveness{Host: "b", Epoch: 9, Live: 4, Total: 4}, b)
}

func TestLivenessChunkFailureStoresNothing(t *testing.T) {
	store := newFakeStore(host("a", 0, 10))
	h := newHarness(t, 1, store, func(o *Options) { o.LivenessChunkSize = 4 })
	h.node.set(func(n *fakeNode) {
		n.livenessStatus = http.StatusInternalServerError
		n.livenessMessage = "state unavailable"
	})

	h.svc.poll(t.Context())

	assert.Empty(t, store.epochs(), "a partial answer is not stored")
	assert.Len(t, h.node.livenessCalls(), 1, "the epoch is abandoned at the first failed chunk")
}

func TestLivenessOverlappingRanges(t *testing.T) {
	store := newFakeStore(host("a", 0, 6), host("b", 4, 10), host("inside-a", 1, 3))
	h := newHarness(t, 1, store, nil)
	h.node.set(func(n *fakeNode) {
		n.minEpoch = 9
		n.isLive = func(_, index uint64) bool { return index >= 2 }
	})

	h.svc.poll(t.Context())

	calls := h.node.livenessCalls()
	require.NotEmpty(t, calls)
	assert.Equal(t, []uint64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, calls[0].indices,
		"shared indices are asked for once")

	a, _ := store.row("a", 9)
	b, _ := store.row("b", 9)
	inside, _ := store.row("inside-a", 9)
	assert.Equal(t, fleet.Liveness{Host: "a", Epoch: 9, Live: 4, Total: 6}, a)
	assert.Equal(t, fleet.Liveness{Host: "b", Epoch: 9, Live: 6, Total: 6}, b)
	assert.Equal(t, fleet.Liveness{Host: "inside-a", Epoch: 9, Live: 1, Total: 2}, inside)

	// Each host counts independently, so the sum counts shared validators twice.
	assert.Equal(t, uint64(14), h.svc.Health().Tracked)

	const warning = "Validator ranges of two hosts overlap"

	assert.Equal(t, 2, h.warnings(warning), "one warning per overlapping pair")

	h.clock.Advance(testEpoch)
	h.node.set(func(n *fakeNode) { n.minEpoch = 10 })
	h.svc.poll(t.Context())

	assert.Equal(t, []uint64{9, 10}, store.epochs())
	assert.Equal(t, 2, h.warnings(warning), "warned once, not every epoch")
}

func TestLivenessOversizedRangeIsSkipped(t *testing.T) {
	store := newFakeStore(host("broken", 0, maxHostValidators+1), host("a", 0, 4))
	h := newHarness(t, 1, store, nil)

	h.svc.poll(t.Context())
	h.clock.Advance(testEpoch)
	h.svc.poll(t.Context())

	_, ok := store.row("broken", 9)
	assert.False(t, ok)
	assert.Equal(t, uint64(4), h.svc.Health().Tracked)
	assert.Equal(t, 1, h.warnings("implausibly large"))
}

func TestEndpointFailoverIsSticky(t *testing.T) {
	h := newHarness(t, 2, newFakeStore(host("a", 0, 4)), nil)
	first, second := h.nodes[0], h.nodes[1]

	first.set(func(n *fakeNode) { n.down = true })
	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.Empty(t, got.Error)
	assert.True(t, got.LivenessKnown)
	assert.Equal(t, 1, first.totalRequests(), "the dead endpoint is tried once per outage")
	assert.Positive(t, second.count(pathHead))

	// The first endpoint is back, but the one that works stays in use.
	first.set(func(n *fakeNode) { n.down = false })
	h.clock.Advance(testEpoch)
	h.svc.poll(t.Context())

	assert.Empty(t, h.svc.Health().Error)
	assert.Equal(t, 1, first.totalRequests())
	assert.Equal(t, 2, second.count(pathHead))

	// The second endpoint dies: back to the first.
	secondBefore := second.totalRequests()

	second.set(func(n *fakeNode) { n.down = true })
	h.clock.Advance(testEpoch)
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.Empty(t, got.Error)
	assert.Equal(t, h.clock.Now(), got.UpdatedAt)
	assert.Equal(t, uint64(11), got.LivenessEpoch)
	assert.Equal(t, secondBefore+1, second.totalRequests())
	assert.Equal(t, 1, first.count(pathHead))

	h.clock.Advance(testSlot)
	h.svc.poll(t.Context())

	assert.Equal(t, secondBefore+1, second.totalRequests(), "sticky on the first again")
	assert.Equal(t, 2, first.count(pathHead))
}

func TestAllEndpointsDown(t *testing.T) {
	h := newHarness(t, 2, newFakeStore(), nil)
	h.nodes[0].set(func(n *fakeNode) { n.down = true })
	h.nodes[1].srv.Close()

	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.True(t, got.UpdatedAt.IsZero())
	assert.Contains(t, got.Error, "HTTP 503")
	assert.Contains(t, got.Error, "connection refused")
	assert.NotContains(t, got.Error, "Get \"", "request URLs are not echoed")
}

func TestLivenessUnsupportedThenRecovers(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
	}{
		{name: "route missing", status: http.StatusNotFound, message: "Not Found"},
		{name: "method not allowed", status: http.StatusMethodNotAllowed},
		{name: "not implemented", status: http.StatusNotImplemented, message: "unimplemented"},
		{
			name:   "teku tracking disabled",
			status: http.StatusBadRequest,
			message: "Validator liveness tracking is not enabled on this beacon node, " +
				"cannot service request",
		},
		{
			name:    "tracking disabled as server error",
			status:  http.StatusServiceUnavailable,
			message: "liveness tracking disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore(host("a", 0, 4))
			// Rows of an earlier run do not make liveness known.
			require.NoError(t, store.SaveLiveness(t.Context(),
				[]fleet.Liveness{{Host: "a", Epoch: 5, Live: 4, Total: 4}}))

			h := newHarness(t, 1, store, nil)
			h.node.set(func(n *fakeNode) {
				n.livenessStatus = tt.status
				n.livenessMessage = tt.message
			})

			h.svc.poll(t.Context())

			got := h.svc.Health()
			assert.False(t, got.LivenessKnown)
			assert.True(t, got.LivenessUnsupported)
			assert.Contains(t, got.Error, "validator liveness unavailable")
			assert.Equal(t, h.clock.Now(), got.UpdatedAt, "the chain view itself is fresh")
			assert.Equal(t, uint64(2), got.FinalityLagEpochs)
			assert.Len(t, h.node.livenessCalls(), 1, "one probe, no backfill attempts")
			assert.Equal(t, 1, h.warnings("does not serve validator liveness"))

			// Not asked again before the re-probe interval, epoch change or not.
			h.clock.Advance(reprobeInterval - time.Second)
			h.svc.poll(t.Context())

			got = h.svc.Health()
			assert.False(t, got.LivenessKnown)
			assert.True(t, got.LivenessUnsupported, "the detection stays in effect between probes")
			assert.Contains(t, got.Error, "validator liveness unavailable")
			assert.Equal(t, h.clock.Now(), got.UpdatedAt)
			assert.Len(t, h.node.livenessCalls(), 1)

			// Re-probed, still unsupported: one more request, one warning in total.
			h.clock.Advance(time.Second)
			h.svc.poll(t.Context())

			assert.False(t, h.svc.Health().LivenessKnown)
			assert.True(t, h.svc.Health().LivenessUnsupported)
			assert.Len(t, h.node.livenessCalls(), 2)
			assert.Equal(t, 1, h.warnings("does not serve validator liveness"))

			// The operator enables tracking: the next probe recovers.
			h.node.set(func(n *fakeNode) { n.livenessStatus = 0 })
			h.clock.Advance(reprobeInterval)
			h.svc.poll(t.Context())

			got = h.svc.Health()
			assert.True(t, got.LivenessKnown)
			assert.False(t, got.LivenessUnsupported, "cleared when the endpoint answers")
			assert.Empty(t, got.Error)
			assert.Equal(t, got.CurrentEpoch-1, got.LivenessEpoch)
			assert.Equal(t, uint64(4), got.Tracked)
			assert.Equal(t, uint64(4), got.Live)
		})
	}
}

func TestLivenessTransientFailureIsRetried(t *testing.T) {
	store := newFakeStore(host("a", 0, 4))
	h := newHarness(t, 1, store, nil)
	h.node.set(func(n *fakeNode) {
		n.livenessStatus = http.StatusInternalServerError
		n.livenessMessage = "state unavailable"
	})

	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.False(t, got.LivenessKnown, "nothing stored yet")
	assert.False(t, got.LivenessUnsupported, "a transient failure is not an unsupported endpoint")
	assert.Contains(t, got.Error, "validator liveness for epoch 9")
	assert.Contains(t, got.Error, "HTTP 500: state unavailable")
	assert.Equal(t, h.clock.Now(), got.UpdatedAt)
	assert.Zero(t, h.warnings("does not serve validator liveness"))

	// Retried on the very next poll, no re-probe wait.
	h.node.set(func(n *fakeNode) { n.livenessStatus = 0 })
	h.clock.Advance(testSlot)
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.True(t, got.LivenessKnown)
	assert.Empty(t, got.Error)
	assert.Equal(t, uint64(9), got.LivenessEpoch)
}

func TestLivenessRejectedNewestEpochIsTransient(t *testing.T) {
	// A 400 for the newest epoch (not a "disabled" message) is neither
	// "unsupported" nor a reason to ask another endpoint the same thing.
	h := newHarness(t, 2, newFakeStore(host("a", 0, 4)), nil)
	h.nodes[0].set(func(n *fakeNode) {
		n.livenessStatus = http.StatusBadRequest
		n.livenessMessage = "validator index unknown"
	})

	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.False(t, got.LivenessKnown)
	assert.False(t, got.LivenessUnsupported)
	assert.Contains(t, got.Error, "HTTP 400: validator index unknown")
	assert.NotContains(t, got.Error, "unavailable")
	assert.Empty(t, h.nodes[1].livenessCalls())
	assert.Len(t, h.nodes[0].livenessCalls(), 1)

	h.clock.Advance(testSlot)
	h.svc.poll(t.Context())

	assert.Len(t, h.nodes[0].livenessCalls(), 2, "retried on the next poll")
}

func TestLivenessFailsOverToEndpointThatServesIt(t *testing.T) {
	store := newFakeStore(host("a", 0, 4))
	h := newHarness(t, 2, store, nil)
	h.nodes[0].set(func(n *fakeNode) { n.livenessStatus = http.StatusNotFound })

	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.True(t, got.LivenessKnown)
	assert.Empty(t, got.Error)
	assert.Equal(t, uint64(9), got.LivenessEpoch)
	assert.Len(t, h.nodes[0].livenessCalls(), 1)
	assert.NotEmpty(t, h.nodes[1].livenessCalls())
}

func TestLivenessBackfill(t *testing.T) {
	t.Run("node serves older epochs", func(t *testing.T) {
		store := newFakeStore(host("a", 0, 4))
		h := newHarness(t, 1, store, nil)
		h.node.set(func(n *fakeNode) {
			n.isLive = func(epoch, index uint64) bool { return epoch != 8 || index == 0 }
		})

		h.svc.poll(t.Context())

		assert.Equal(t, []uint64{9, 8, 7}, h.node.livenessEpochs(), "newest first")
		assert.Equal(t, []uint64{7, 8, 9}, store.epochs())

		old, _ := store.row("a", 8)
		assert.Equal(t, fleet.Liveness{Host: "a", Epoch: 8, Live: 1, Total: 4}, old)

		got := h.svc.Health()
		assert.Equal(t, uint64(9), got.LivenessEpoch)
		assert.Equal(t, uint64(4), got.Live)
	})

	t.Run("node serves the previous epoch only", func(t *testing.T) {
		store := newFakeStore(host("a", 0, 4))
		h := newHarness(t, 1, store, nil)
		h.node.set(func(n *fakeNode) { n.minEpoch = 9 })

		h.svc.poll(t.Context())

		assert.Equal(t, []uint64{9, 8, 7}, h.node.livenessEpochs())
		assert.Equal(t, []uint64{9}, store.epochs())

		got := h.svc.Health()
		assert.Empty(t, got.Error, "refused old epochs are not an error")
		assert.True(t, got.LivenessKnown)
		assert.Zero(t, h.warnings(""), "and are handled quietly")

		// The refused epochs are not asked for again.
		h.clock.Advance(testSlot)
		h.svc.poll(t.Context())

		assert.Len(t, h.node.livenessCalls(), 3)
	})

	// Not every client refuses an out-of-range epoch with 400.
	for _, status := range []int{
		http.StatusInternalServerError, http.StatusServiceUnavailable,
		http.StatusUnprocessableEntity, http.StatusNotFound,
	} {
		t.Run("node answers older epochs with "+http.StatusText(status), func(t *testing.T) {
			store := newFakeStore(host("a", 0, 4))
			h := newHarness(t, 1, store, nil)
			h.node.set(func(n *fakeNode) {
				n.minEpoch = 9
				n.oldEpochStatus = status
			})

			h.svc.poll(t.Context())

			assert.Equal(t, []uint64{9, 8, 7}, h.node.livenessEpochs())
			assert.Equal(t, []uint64{9}, store.epochs())

			got := h.svc.Health()
			assert.Empty(t, got.Error, "an unserved old epoch is not an error")
			assert.True(t, got.LivenessKnown)
			assert.Equal(t, uint64(9), got.LivenessEpoch)
			assert.Zero(t, h.warnings(""), "and is handled quietly")

			// The older epochs are not asked for again.
			h.clock.Advance(testSlot)
			h.svc.poll(t.Context())

			assert.Len(t, h.node.livenessCalls(), 3)
			assert.Empty(t, h.svc.Health().Error)
			assert.Zero(t, h.warnings(""))
		})
	}

	t.Run("early chain has fewer epochs than the window", func(t *testing.T) {
		store := newFakeStore(host("a", 0, 4))
		h := newHarness(t, 1, store, nil)
		h.clock.Set(at(2, 30*time.Second))

		h.svc.poll(t.Context())

		assert.Equal(t, []uint64{1, 0}, h.node.livenessEpochs())
		assert.Equal(t, []uint64{0, 1}, store.epochs())
	})

	t.Run("an epoch missed during an outage is fetched afterwards", func(t *testing.T) {
		store := newFakeStore(host("a", 0, 4))
		h := newHarness(t, 1, store, nil)
		h.svc.poll(t.Context())
		require.Equal(t, []uint64{7, 8, 9}, store.epochs())

		h.node.set(func(n *fakeNode) { n.down = true })
		h.clock.Advance(testEpoch)
		h.svc.poll(t.Context())
		require.NotEmpty(t, h.svc.Health().Error)

		h.node.set(func(n *fakeNode) { n.down = false })
		h.clock.Advance(testEpoch)
		h.svc.poll(t.Context())

		assert.Equal(t, []uint64{7, 8, 9, 10, 11}, store.epochs())
		assert.Equal(t, []uint64{9, 8, 7, 11, 10}, h.node.livenessEpochs())
	})
}

func TestLivenessWaitsForTheEpochToSettle(t *testing.T) {
	store := newFakeStore(host("a", 0, 4))
	h := newHarness(t, 1, store, nil)
	h.node.set(func(n *fakeNode) { n.minEpoch = 8 })

	// 5 s into epoch 10: epoch 9 ended less than a slot ago.
	h.clock.Set(at(10, 5*time.Second))
	h.svc.poll(t.Context())

	assert.Equal(t, []uint64{8}, store.epochs())
	assert.Equal(t, uint64(8), h.svc.Health().LivenessEpoch)

	h.clock.Set(at(10, testSlot))
	h.svc.poll(t.Context())

	assert.Equal(t, []uint64{8, 9}, store.epochs())
	assert.Equal(t, uint64(9), h.svc.Health().LivenessEpoch)
}

func TestLivenessStoredEpochIsNotQueriedAgain(t *testing.T) {
	store := newFakeStore(host("a", 0, 4))
	h := newHarness(t, 1, store, nil)
	h.node.set(func(n *fakeNode) { n.minEpoch = 9 })

	h.svc.poll(t.Context())
	require.Equal(t, []uint64{9, 8, 7}, h.node.livenessEpochs())

	hostReads := store.listHostsCalls

	// More polls inside the same epoch ask for nothing and read no hosts.
	for range 5 {
		h.clock.Advance(testSlot)
		h.svc.poll(t.Context())
	}

	assert.Len(t, h.node.livenessCalls(), 3)
	assert.Equal(t, hostReads, store.listHostsCalls)
	assert.Equal(t, 1, store.saveCalls)

	// A restarted controller finds epoch 9 in the store and does not repeat
	// it; the older ones are probed once by the new process.
	restarted := h.build(t)
	restarted.poll(t.Context())

	assert.Equal(t, []uint64{9, 8, 7, 8, 7}, h.node.livenessEpochs())
	assert.Equal(t, 1, store.saveCalls)
	assert.True(t, restarted.Health().LivenessKnown)
	assert.Equal(t, uint64(9), restarted.Health().LivenessEpoch)

	// The next epoch is queried exactly once.
	h.clock.Set(at(11, 30*time.Second))
	h.node.set(func(n *fakeNode) { n.minEpoch = 10 })
	restarted.poll(t.Context())
	restarted.poll(t.Context())

	assert.Equal(t, []uint64{9, 8, 7, 8, 7, 10}, h.node.livenessEpochs())
	assert.Equal(t, []uint64{9, 10}, store.epochs())
	assert.Equal(t, hostReads+2, store.listHostsCalls, "hosts are read once per epoch")
}

func TestLivenessHostsAreReadEveryEpoch(t *testing.T) {
	store := newFakeStore(host("a", 0, 4))
	h := newHarness(t, 1, store, nil)
	h.node.set(func(n *fakeNode) { n.minEpoch = 9 })

	h.svc.poll(t.Context())
	require.Equal(t, uint64(4), h.svc.Health().Tracked)

	store.set(func(s *fakeStore) { s.hosts = append(s.hosts, host("new", 4, 12)) })
	h.clock.Advance(testEpoch)
	h.node.set(func(n *fakeNode) { n.minEpoch = 10 })
	h.svc.poll(t.Context())

	added, ok := store.row("new", 10)
	require.True(t, ok)
	assert.Equal(t, fleet.Liveness{Host: "new", Epoch: 10, Live: 8, Total: 8}, added)

	_, ok = store.row("new", 9)
	assert.False(t, ok)

	got := h.svc.Health()
	assert.Equal(t, uint64(10), got.LivenessEpoch)
	assert.Equal(t, uint64(12), got.Tracked)
}

func TestLivenessWithoutValidators(t *testing.T) {
	store := newFakeStore(host("bootnode-1", 0, 0), host("rpc-1", 0, 0))
	h := newHarness(t, 1, store, nil)
	h.node.set(func(n *fakeNode) { n.minEpoch = 9 })

	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.Empty(t, got.Error)
	assert.False(t, got.LivenessKnown, "nothing is stored, so nothing is known")
	assert.Zero(t, got.Tracked)
	assert.InDelta(t, 1.0, got.Participation(), 1e-9)
	assert.Empty(t, h.node.livenessCalls())

	// The first validator host registers within the epoch.
	store.set(func(s *fakeStore) { s.hosts = append(s.hosts, host("validator-1", 0, 8)) })
	h.clock.Advance(testSlot)
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.True(t, got.LivenessKnown)
	assert.Equal(t, uint64(9), got.LivenessEpoch)
	assert.Equal(t, uint64(8), got.Tracked)
}

func TestLivenessValidatorsRemoved(t *testing.T) {
	store := newFakeStore(host("a", 0, 4), host("rpc-1", 0, 0))
	h := newHarness(t, 1, store, nil)
	h.node.set(func(n *fakeNode) { n.minEpoch = 9 })

	h.svc.poll(t.Context())

	got := h.svc.Health()
	require.True(t, got.LivenessKnown)
	require.Equal(t, uint64(9), got.LivenessEpoch)
	require.Equal(t, uint64(4), got.Tracked)

	// The only validator host loses its validators. Epoch 9 is stored and
	// still the newest complete epoch: its summary stands.
	store.set(func(s *fakeStore) { s.hosts = []*fleet.Host{host("a", 0, 0), host("rpc-1", 0, 0)} })
	h.clock.Advance(testSlot)
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.True(t, got.LivenessKnown)
	assert.Equal(t, uint64(4), got.Tracked)

	// From the next epoch on nothing is tracked; the stored rows of epoch 9
	// must not be reported as the fleet's liveness.
	for range 3 {
		h.clock.Advance(testEpoch)
		h.svc.poll(t.Context())

		got = h.svc.Health()
		assert.Empty(t, got.Error)
		assert.False(t, got.LivenessKnown, "nothing is tracked, so nothing is known")
		assert.Zero(t, got.LivenessEpoch)
		assert.Zero(t, got.Tracked)
		assert.Zero(t, got.Live)
		assert.InDelta(t, 1.0, got.Participation(), 1e-9)
	}

	assert.Equal(t, []uint64{9}, store.epochs(), "the old rows stay until they are pruned")
	assert.Len(t, h.node.livenessCalls(), 3, "nothing was asked without validators")

	// A validator host is back: its newest epoch is reported again.
	store.set(func(s *fakeStore) { s.hosts = []*fleet.Host{host("b", 10, 16)} })
	h.node.set(func(n *fakeNode) { n.minEpoch = 12 })
	h.clock.Advance(testSlot)
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.True(t, got.LivenessKnown)
	assert.Equal(t, uint64(12), got.LivenessEpoch)
	assert.Equal(t, uint64(6), got.Tracked)
	assert.Equal(t, uint64(6), got.Live)
}

func TestLivenessPruning(t *testing.T) {
	store := newFakeStore(host("a", 0, 4))
	for epoch := uint64(0); epoch < 9; epoch++ {
		require.NoError(t, store.SaveLiveness(t.Context(),
			[]fleet.Liveness{{Host: "a", Epoch: epoch, Live: 4, Total: 4}}))
	}

	h := newHarness(t, 1, store, func(o *Options) { o.Retention = 4 })
	h.node.set(func(n *fakeNode) { n.minEpoch = 9 })

	h.svc.poll(t.Context())

	assert.Equal(t, []uint64{6, 7, 8, 9}, store.epochs())
	assert.Equal(t, []uint64{6}, store.pruneCalls)

	h.clock.Advance(testSlot)
	h.svc.poll(t.Context())

	assert.Equal(t, []uint64{6}, store.pruneCalls, "pruned once per epoch")

	h.clock.Set(at(11, 30*time.Second))
	h.node.set(func(n *fakeNode) { n.minEpoch = 10 })
	h.svc.poll(t.Context())

	assert.Equal(t, []uint64{7, 8, 9, 10}, store.epochs())
	assert.Equal(t, []uint64{6, 7}, store.pruneCalls)
}

func TestLivenessPruningDefaultsAndYoungChain(t *testing.T) {
	store := newFakeStore(host("a", 0, 4))
	h := newHarness(t, 1, store, nil)

	h.svc.poll(t.Context())
	assert.Empty(t, store.pruneCalls, "younger than the retention: nothing to prune")

	h.clock.Set(at(DefaultRetention+6, 30*time.Second))
	h.svc.poll(t.Context())
	assert.Equal(t, []uint64{6}, store.pruneCalls)
}

func TestStoreFailures(t *testing.T) {
	t.Run("listing hosts", func(t *testing.T) {
		store := newFakeStore(host("a", 0, 4))
		store.listHostsErr = errStore
		h := newHarness(t, 1, store, nil)

		h.svc.poll(t.Context())

		got := h.svc.Health()
		assert.Contains(t, got.Error, "listing hosts: database is locked")
		assert.Equal(t, h.clock.Now(), got.UpdatedAt)
		assert.False(t, got.LivenessKnown)

		store.set(func(s *fakeStore) { s.listHostsErr = nil })
		h.svc.poll(t.Context())

		assert.Empty(t, h.svc.Health().Error)
		assert.True(t, h.svc.Health().LivenessKnown)
	})

	t.Run("saving rows", func(t *testing.T) {
		store := newFakeStore(host("a", 0, 4))
		store.saveErr = errStore
		h := newHarness(t, 1, store, nil)
		h.node.set(func(n *fakeNode) { n.minEpoch = 9 })

		h.svc.poll(t.Context())

		assert.Contains(t, h.svc.Health().Error, "saving liveness for epoch 9")
		assert.False(t, h.svc.Health().LivenessKnown)

		// The epoch was not marked as done: it is asked for and saved again.
		store.set(func(s *fakeStore) { s.saveErr = nil })
		h.svc.poll(t.Context())

		assert.Empty(t, h.svc.Health().Error)
		assert.Equal(t, []uint64{9}, store.epochs())
	})

	t.Run("reading rows", func(t *testing.T) {
		store := newFakeStore(host("a", 0, 4))
		h := newHarness(t, 1, store, nil)
		h.svc.poll(t.Context())
		require.True(t, h.svc.Health().LivenessKnown)

		store.set(func(s *fakeStore) { s.listErr = errStore })
		h.clock.Advance(testSlot)
		h.svc.poll(t.Context())

		got := h.svc.Health()
		assert.Contains(t, got.Error, "reading stored liveness: database is locked")
		assert.True(t, got.LivenessKnown, "the last known summary stays")
		assert.Equal(t, uint64(4), got.Tracked)
	})

	t.Run("reading rows on the first poll", func(t *testing.T) {
		store := newFakeStore(host("a", 0, 4))
		store.listErr = errStore
		h := newHarness(t, 1, store, nil)

		h.svc.poll(t.Context())

		assert.Contains(t, h.svc.Health().Error, "reading stored liveness")
		assert.Empty(t, h.node.livenessCalls(), "nothing is queried before the store was read")

		store.set(func(s *fakeStore) { s.listErr = nil })
		h.svc.poll(t.Context())

		assert.Empty(t, h.svc.Health().Error)
		assert.True(t, h.svc.Health().LivenessKnown)
	})
}

func TestMalformedAnswers(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		body    string
		wantErr string
	}{
		{name: "genesis without time", path: pathGenesis, body: `{"data":{}}`,
			wantErr: "no genesis_time"},
		{name: "genesis not json", path: pathGenesis, body: `<html>`,
			wantErr: "invalid JSON answer"},
		{name: "head without slot", path: pathHead, body: `{"data":{"header":{}}}`,
			wantErr: "no slot"},
		{name: "head slot not a number", path: pathHead,
			body:    `{"data":{"header":{"message":{"slot":"0x10"}}}}`,
			wantErr: "not an unsigned integer"},
		{name: "finality without epoch", path: pathFinality, body: `{"data":{"finalized":{}}}`,
			wantErr: "no finalized epoch"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, 1, newFakeStore(), nil)

			// A proxy in front of the node answers one path with garbage.
			proxy := http.NewServeMux()
			proxy.HandleFunc(tt.path, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			})
			proxy.HandleFunc("/", h.node.handle)
			h.node.srv.Config.Handler = proxy

			h.svc.poll(t.Context())

			got := h.svc.Health()
			assert.Contains(t, got.Error, tt.wantErr)
			assert.True(t, got.UpdatedAt.IsZero())
		})
	}
}

func TestStartValidatesOptions(t *testing.T) {
	log, _ := logtest.NewNullLogger()
	store := newFakeStore()

	tests := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{name: "no endpoints", opts: Options{Store: store}, wantErr: "no beacon endpoints"},
		{name: "no store", opts: Options{Endpoints: []string{"http://beacon:5052"}},
			wantErr: "needs a store"},
		{name: "not http", opts: Options{Store: store, Endpoints: []string{"beacon:5052"}},
			wantErr: "endpoint 1 must be an http(s) URL"},
		{name: "second endpoint invalid", wantErr: "endpoint 2 is not a valid URL",
			opts: Options{Store: store, Endpoints: []string{
				"http://beacon:5052", "http://user:secret@bad host/",
			}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := New(log, tt.opts)

			err := svc.Start(t.Context())
			require.ErrorContains(t, err, tt.wantErr)
			assert.NotContains(t, err.Error(), "secret")
			require.NoError(t, svc.Stop())
		})
	}
}

func TestEndpointURLForms(t *testing.T) {
	// A trailing slash and a path prefix both work.
	h := newHarness(t, 1, newFakeStore(), nil)

	mux := http.NewServeMux()
	mux.Handle("/prefix/", http.StripPrefix("/prefix", http.HandlerFunc(h.node.handle)))
	h.node.srv.Config.Handler = mux

	h.opts.Endpoints = []string{h.node.srv.URL + "/prefix/"}
	svc := h.build(t)

	svc.poll(t.Context())

	assert.Empty(t, svc.Health().Error)
	assert.Equal(t, uint64(82), svc.Health().HeadSlot)
}

func TestStartStopLifecycle(t *testing.T) {
	store := newFakeStore(host("a", 0, 4))
	h := newHarness(t, 1, store, func(o *Options) { o.PollInterval = 2 * time.Millisecond })

	svc := New(h.log, h.opts)

	require.NoError(t, svc.Stop(), "Stop before Start is harmless")
	require.NoError(t, svc.Start(t.Context()))
	require.ErrorContains(t, svc.Start(t.Context()), "already started")

	// Readers hammer Health while the poller writes and the clock moves.
	var (
		readers sync.WaitGroup
		done    = make(chan struct{})
	)

	for range 8 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					health := svc.Health()
					_ = health.Participation()
				}
			}
		})
	}

	readers.Go(func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
				h.clock.Advance(testSlot)
			}
		}
	})

	require.Eventually(t, func() bool {
		health := svc.Health()

		return health.LivenessKnown && health.Error == "" && health.CurrentEpoch >= 12
	}, 10*time.Second, 2*time.Millisecond)

	close(done)
	readers.Wait()

	require.NoError(t, svc.Stop())
	require.NoError(t, svc.Stop(), "Stop twice is harmless")

	// Nothing polls after Stop.
	before := h.node.totalRequests()

	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, before, h.node.totalRequests())

	health := svc.Health()
	assert.Equal(t, testGenesis, health.GenesisTime)
	assert.Equal(t, uint64(4), health.Tracked)
}

func TestContextCancelStopsPolling(t *testing.T) {
	h := newHarness(t, 1, newFakeStore(), func(o *Options) { o.PollInterval = time.Millisecond })

	ctx, cancel := context.WithCancel(t.Context())
	svc := New(h.log, h.opts)
	require.NoError(t, svc.Start(ctx))

	require.Eventually(t, func() bool { return !svc.Health().UpdatedAt.IsZero() },
		10*time.Second, time.Millisecond)

	cancel()

	stopped := make(chan error, 1)
	go func() { stopped <- svc.Stop() }()

	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the context was cancelled")
	}

	before := h.node.totalRequests()

	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, before, h.node.totalRequests())
}

func TestSlowNodeDoesNotBlockHealthOrStop(t *testing.T) {
	h := newHarness(t, 1, newFakeStore(host("a", 0, 4)), nil)

	// One good poll, then the node hangs on every request.
	h.svc.poll(t.Context())
	require.Empty(t, h.svc.Health().Error)

	release := make(chan struct{})
	defer close(release)

	h.node.set(func(n *fakeNode) { n.block = release })

	before := h.node.totalRequests()

	h.svc.lifecycle.Lock()
	ctx, cancel := context.WithCancel(t.Context())
	h.svc.started = true
	h.svc.cancel = cancel
	h.svc.wg.Go(func() { h.svc.run(ctx) })
	h.svc.lifecycle.Unlock()

	require.Eventually(t, func() bool { return h.node.totalRequests() > before },
		10*time.Second, time.Millisecond, "a request is in flight")

	answered := make(chan fleet.NetworkHealth, 1)
	go func() { answered <- h.svc.Health() }()

	select {
	case health := <-answered:
		assert.Equal(t, uint64(82), health.HeadSlot, "the last good view is served")
		assert.Empty(t, health.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("Health blocked on a hanging beacon node")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- h.svc.Stop() }()

	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked on a hanging beacon node")
	}

	assert.Empty(t, h.svc.Health().Error, "a cancelled poll is not reported as a failure")
}

func TestPollIntervalDefaults(t *testing.T) {
	h := newHarness(t, 1, newFakeStore(), nil)

	assert.Equal(t, DefaultPollInterval, h.svc.interval(), "until the slot duration is known")

	h.node.set(func(n *fakeNode) {
		n.spec = map[string]any{"SLOT_DURATION_MS": "6000", "SLOTS_PER_EPOCH": "8"}
	})
	h.svc.poll(t.Context())
	assert.Equal(t, 6*time.Second, h.svc.interval(), "one slot")

	h.svc.opts.PollInterval = time.Minute
	assert.Equal(t, time.Minute, h.svc.interval())

	fast := newHarness(t, 1, newFakeStore(), nil)
	fast.node.set(func(n *fakeNode) {
		n.spec = map[string]any{"SLOT_DURATION_MS": "100", "SLOTS_PER_EPOCH": "8"}
	})
	fast.svc.poll(t.Context())
	assert.Equal(t, minPollInterval, fast.svc.interval())
}
