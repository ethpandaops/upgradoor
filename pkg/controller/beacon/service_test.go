package beacon

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
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
		Phases:            fleet.SinglePhase(testGenesis, 12*time.Second),
		HeadSlot:          82,
		CurrentEpoch:      10,
		FinalizedEpoch:    8,
		FinalityLagEpochs: 2,
		// No host has validators: epoch 9 is settled with nothing tracked.
		LivenessKnown: true,
		LivenessEpoch: 9,
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
				assert.False(t, got.LivenessKnown, "no complete epoch yet")
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

// TestPollPublishesTheChainTiming: the timing the service reads is published
// as slot phases that are valid as a whole (fleet.ValidateTiming), and the
// view places every epoch where the service does, across forks that change
// the slot duration.
func TestPollPublishesTheChainTiming(t *testing.T) {
	for _, tt := range []struct {
		name string
		spec map[string]any
	}{
		{
			name: "mainnet", spec: map[string]any{"SECONDS_PER_SLOT": "12", "SLOTS_PER_EPOCH": "32"},
		},
		{
			name: "minimal", spec: map[string]any{"SECONDS_PER_SLOT": "6", "SLOTS_PER_EPOCH": "8"},
		},
		{
			name: "gnosis", spec: map[string]any{"SECONDS_PER_SLOT": "5", "SLOTS_PER_EPOCH": "16"},
		},
		{
			name: "a fork to 6 s slots that has passed",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOT_DURATION_MS": "12000", "SLOTS_PER_EPOCH": "32",
				"SLOT_DURATION_MS_GLOAS": "6000", "GLOAS_FORK_EPOCH": "7",
			},
		},
		{
			name: "forks to 4.5 s and then 3 s slots",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOTS_PER_EPOCH": "32", "SLOT_DURATION_MS_GLOAS": "4500",
				"GLOAS_FORK_EPOCH": "7", "SLOT_DURATION_MS_HEZE": "3000", "HEZE_FORK_EPOCH": "13",
			},
		},
		{
			name: "a fork that is not scheduled",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOTS_PER_EPOCH": "32",
				"SLOT_DURATION_MS_GLOAS": "6000", "GLOAS_FORK_EPOCH": "18446744073709551615",
			},
		},
		{
			name: "a valid fork 190 years out",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOTS_PER_EPOCH": "32",
				"SLOT_DURATION_MS_GLOAS": "6000", "GLOAS_FORK_EPOCH": "15600000",
			},
		},
		{
			name: "long slots from a fork far out",
			spec: map[string]any{
				"SECONDS_PER_SLOT": "12", "SLOTS_PER_EPOCH": "32",
				"SLOT_DURATION_MS_GLOAS": "7200000", "GLOAS_FORK_EPOCH": "1000000",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, 1, newFakeStore(host("a", 0, 4)), nil)
			h.node.set(func(n *fakeNode) {
				n.spec = tt.spec
				n.headSlot = 0
				n.finalized = 0
			})

			h.svc.poll(t.Context())
			h.clock.Advance(testSlot)
			h.svc.poll(t.Context())

			got := h.svc.Health()
			assert.Empty(t, got.Error, "the node answered")
			assert.False(t, got.UpdatedAt.IsZero())

			assert.Zero(t, h.warnings(timingWarning))
			require.NoError(t, fleet.ValidateTiming(got.SlotsPerEpoch, got.Phases))
			assert.Empty(t, got.TimingError)
			assert.Positive(t, got.EpochDuration())
		})
	}
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

func TestStoreFailuresHoldTheLivenessBack(t *testing.T) {
	store := newFakeStore(host("a", 0, 4))
	store.listHostsErr = errStore
	h := newHarness(t, 1, store, nil)

	h.svc.poll(t.Context())

	got := h.svc.Health()
	assert.Empty(t, got.Error, "the chain view does not need the store")
	assert.Equal(t, h.clock.Now(), got.UpdatedAt)
	assert.Contains(t, got.LivenessError, "listing hosts: database is locked")
	assert.False(t, got.LivenessKnown)

	store.set(func(s *fakeStore) {
		s.listHostsErr = nil
		s.slotSaveErr = errStore
	})
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.Contains(t, got.LivenessError, "saving slot participation: database is locked")
	assert.False(t, got.LivenessKnown, "slots that were not stored settle no epoch")

	store.set(func(s *fakeStore) { s.slotSaveErr = nil })
	h.svc.poll(t.Context())

	got = h.svc.Health()
	assert.Empty(t, got.LivenessError)
	assert.True(t, got.LivenessKnown)
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

	// Nothing polls after Stop. A request the cancelled poll had already sent
	// may still reach the node's handler after Stop returned: let it land
	// before counting.
	time.Sleep(20 * time.Millisecond)

	before := h.node.totalRequests()

	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, before, h.node.totalRequests())

	health := svc.Health()
	assert.Equal(t, testGenesis, health.GenesisTime)
	assert.True(t, health.LivenessKnown)
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

// TestLiveBeaconNode runs one poll against a real beacon node. It only reads
// (a handful of GETs: genesis, spec, head, finality, blocks and committees) and is skipped
// unless UPGRADOOR_BEACON_URL is set:
//
//	UPGRADOOR_BEACON_URL=https://beacon.example.org go test -run TestLiveBeaconNode -v ./pkg/controller/beacon/
func TestLiveBeaconNode(t *testing.T) {
	endpoint := os.Getenv("UPGRADOOR_BEACON_URL")
	if endpoint == "" {
		t.Skip("UPGRADOOR_BEACON_URL is not set")
	}

	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)

	store := newFakeStore(host("smoke-a", 0, 8), host("smoke-b", 8, 16), host("no-validators", 0, 0))

	svc, ok := New(log, Options{Endpoints: []string{endpoint}, Store: store}).(*service)
	require.True(t, ok)
	require.NoError(t, svc.init())

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	svc.poll(ctx)

	health := svc.Health()
	t.Logf("health: %+v", health)

	require.False(t, health.UpdatedAt.IsZero(), "poll failed: %s", health.Error)
	assert.False(t, health.GenesisTime.IsZero())
	assert.Positive(t, health.SecondsPerSlot)
	assert.Positive(t, health.SlotsPerEpoch)
	assert.Positive(t, health.HeadSlot)
	assert.GreaterOrEqual(t, health.CurrentEpoch, health.FinalizedEpoch)
	assert.Equal(t, health.CurrentEpoch-health.FinalizedEpoch, health.FinalityLagEpochs)

	// The wall-clock slot must agree with the head of a live chain; this is
	// what proves the slot-duration maths against a real spec.
	clockSlot := svc.timing.slotAt(time.Now())
	assert.InDelta(t, float64(clockSlot), float64(health.HeadSlot), float64(2*health.SlotsPerEpoch),
		"wall-clock slot %d is far from head slot %d", clockSlot, health.HeadSlot)

	// The first poll reads the previous epoch from its first slot on, so it
	// is settled unless the clock is in the first slots of an epoch.
	if !health.LivenessKnown {
		t.Logf("no settled epoch yet: %s", health.LivenessError)

		return
	}

	assert.Empty(t, health.LivenessError)
	assert.LessOrEqual(t, health.Tracked, uint64(16), "one duty per validator and epoch")
	assert.LessOrEqual(t, health.Live, health.Tracked)
	assert.GreaterOrEqual(t, health.LivenessEpoch+2, health.CurrentEpoch)

	for _, row := range svc.Liveness(1) {
		t.Logf("%s: %d/%d live in epoch %d", row.Host, row.Live, row.Total, row.Epoch)
	}
}
