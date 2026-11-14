package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func needsConfig(verbs ...protocol.Verb) *Config {
	return &Config{
		Verbs: verbs,
		Containers: []ManagedContainer{
			{Name: "geth", Kind: protocol.KindEL, Image: "ethpandaops/geth:master"},
			{Name: "teku", Kind: protocol.KindCL, Image: "consensys/teku:develop"},
		},
		Packages: PackagesConfig{Blocked: []string{"docker-ce", "containerd.io"}},
	}
}

var allVerbs = []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot}

// observed builds an observation in which geth has a pulled new image,
// teku is up to date, docker-ce and an unblocked package can be upgraded
// and both reboot reasons apply.
func observed(cfg *Config) *observation {
	containers := []protocol.Container{
		{Name: "geth", Image: "ethpandaops/geth:master", Digest: "sha256:a", State: containerRunning},
		{Name: "teku", Image: "consensys/teku:develop", Digest: "sha256:t", State: containerRunning},
		{Name: "dora", Image: "ethpandaops/dora:latest", Digest: "sha256:d", State: containerRunning},
	}

	checks := map[string]protocol.RegistryCheck{
		"docker.io/ethpandaops/geth:master": {Digest: "sha256:b", Revision: "r2", Pulled: true},
		"docker.io/consensys/teku:develop":  {Digest: "sha256:t", Pulled: true},
		"docker.io/ethpandaops/dora:latest": {Digest: "sha256:e", Pulled: true},
	}

	enrichContainers(cfg, containers, func(key string) (protocol.RegistryCheck, bool) {
		c, ok := checks[key]

		return c, ok
	})

	return &observation{
		containers:      containers,
		containersFresh: true,
		sys: &SystemStatus{
			OS: protocol.OSSummary{
				RunningKernel: "6.12.41", NewestKernel: "6.12.48", RebootRequired: true,
			},
			Blocked: []protocol.PackageChange{
				{Name: "docker-ce", From: "1", To: "2"},
				// Reported by the system but not in the configured set.
				{Name: "openssl", From: "1", To: "2"},
				{Name: "containerd.io", From: "3", To: "4"},
			},
		},
	}
}

func ids(needs []protocol.Need) []string {
	out := make([]string, 0, len(needs))
	for _, n := range needs {
		out = append(out, n.ID)
	}

	return out
}

func TestComputeNeeds(t *testing.T) {
	pkgs := []protocol.PackageChange{
		{Name: "containerd.io", From: "3", To: "4"},
		{Name: "docker-ce", From: "1", To: "2"},
	}
	imageID := protocol.ImageNeedID("geth", "sha256:b")
	packagesID := protocol.PackagesNeedID(pkgs)

	manualRoll := protocol.Need{
		ID: "manual/d-roll", Kind: protocol.NeedImage, DirectiveID: "d-roll",
		Image: &protocol.ImageNeed{Container: "geth", Image: "ethpandaops/geth:master", ToDigest: "sha256:b"},
	}
	manualReboot := protocol.Need{
		ID: "manual/d-reboot", Kind: protocol.NeedReboot, DirectiveID: "d-reboot",
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}

	tests := []struct {
		name   string
		verbs  []protocol.Verb
		mutate func(obs *observation)
		manual []protocol.Need
		want   []string
	}{
		{
			name:  "everything observed",
			verbs: allVerbs,
			want:  []string{imageID, packagesID, protocol.RebootNeedID},
		},
		{
			name:   "any hold: no needs at all",
			verbs:  allVerbs,
			mutate: func(obs *observation) { obs.hold.Local = true },
			manual: []protocol.Need{manualRoll, manualReboot},
			want:   []string{},
		},
		{
			name:   "controller hold too",
			verbs:  allVerbs,
			mutate: func(obs *observation) { obs.hold.Controller = true },
			want:   []string{},
		},
		{
			name:  "only the allowed verbs",
			verbs: []protocol.Verb{protocol.VerbPackages},
			want:  []string{packagesID},
		},
		{
			name:   "no verbs, no needs, not even manual ones",
			manual: []protocol.Need{manualRoll, manualReboot},
			want:   []string{},
		},
		{
			name:   "image not pulled yet",
			verbs:  []protocol.Verb{protocol.VerbRoll},
			mutate: func(obs *observation) { obs.containers[0].Registry.Pulled = false },
			want:   []string{},
		},
		{
			name:   "registry never answered",
			verbs:  []protocol.Verb{protocol.VerbRoll},
			mutate: func(obs *observation) { obs.containers[0].Registry = nil },
			want:   []string{},
		},
		{
			name:  "custom container",
			verbs: []protocol.Verb{protocol.VerbRoll},
			mutate: func(obs *observation) {
				obs.containers[0].Custom = true
			},
			want: []string{},
		},
		{
			name:  "kernel only",
			verbs: []protocol.Verb{protocol.VerbReboot},
			mutate: func(obs *observation) {
				obs.sys.OS.RebootRequired = false
			},
			want: []string{protocol.RebootNeedID},
		},
		{
			name:  "no reboot reason",
			verbs: []protocol.Verb{protocol.VerbReboot},
			mutate: func(obs *observation) {
				obs.sys.OS.RebootRequired = false
				obs.sys.OS.NewestKernel = obs.sys.OS.RunningKernel
			},
			want: []string{},
		},
		{
			name:   "system state unknown",
			verbs:  allVerbs,
			mutate: func(obs *observation) { obs.sys = nil },
			want:   []string{imageID},
		},
		{
			name:   "a manual need replaces the observed one for the same thing",
			verbs:  allVerbs,
			manual: []protocol.Need{manualRoll, manualReboot},
			want:   []string{"manual/d-reboot", "manual/d-roll", packagesID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := needsConfig(tt.verbs...)
			obs := observed(cfg)

			if tt.mutate != nil {
				tt.mutate(obs)
			}

			got := computeNeeds(cfg, obs, tt.manual)
			require.NotNil(t, got, "no needs is an empty list, not null")
			assert.Equal(t, tt.want, ids(got))
		})
	}

	// The content of the observed needs.
	cfg := needsConfig(allVerbs...)
	got := computeNeeds(cfg, observed(cfg), nil)
	require.Len(t, got, 3)
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Image: "ethpandaops/geth:master",
		FromDigest: "sha256:a", ToDigest: "sha256:b", ToRevision: "r2",
	}, got[0].Image)
	assert.Equal(t, pkgs, got[1].Packages.Packages, "sorted, and only the configured blocked set")
	assert.Equal(t, []string{protocol.RebootReasonFlag, protocol.RebootReasonKernel}, got[2].Reboot.Reasons)
}

func TestReconcileManualNeeds(t *testing.T) {
	cfg := needsConfig(allVerbs...)

	roll := func(container, image, to string) protocol.Need {
		return protocol.Need{
			ID: "manual/" + container, Kind: protocol.NeedImage, DirectiveID: container,
			Image: &protocol.ImageNeed{Container: container, Image: image, FromDigest: "sha256:old", ToDigest: to},
		}
	}
	reboot := protocol.Need{
		ID: "manual/reboot", Kind: protocol.NeedReboot, DirectiveID: "reboot",
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}

	t.Run("a manual roll follows its tag", func(t *testing.T) {
		manual := []protocol.Need{roll("geth", "ethpandaops/geth:master", "sha256:a"), reboot}

		kept, dropped, changed := reconcileManualNeeds(cfg, observed(cfg), manual)
		assert.True(t, changed)
		assert.Empty(t, dropped)
		require.Len(t, kept, 2)
		assert.Equal(t, "sha256:a", kept[0].Image.FromDigest, "the running digest")
		assert.Equal(t, "sha256:b", kept[0].Image.ToDigest, "the tag's current, pulled digest")
		assert.Equal(t, "r2", kept[0].Image.ToRevision)
		assert.Equal(t, "sha256:a", manual[0].Image.ToDigest, "the input is not modified")
	})

	t.Run("nothing to do", func(t *testing.T) {
		manual := []protocol.Need{reboot}

		kept, dropped, changed := reconcileManualNeeds(cfg, observed(cfg), manual)
		assert.False(t, changed)
		assert.Empty(t, dropped)
		assert.Equal(t, manual, kept)
	})

	t.Run("a tag that is not pulled yet is not followed", func(t *testing.T) {
		obs := observed(cfg)
		obs.containers[0].Registry.Pulled = false
		obs.containers[0].Digest = "sha256:old"

		kept, _, changed := reconcileManualNeeds(cfg, obs,
			[]protocol.Need{roll("geth", "ethpandaops/geth:master", "sha256:a")})
		assert.False(t, changed)
		assert.Equal(t, "sha256:a", kept[0].Image.ToDigest)
	})

	drops := []struct {
		name   string
		mutate func(cfg *Config, obs *observation)
		need   protocol.Need
		reason string
	}{
		{
			name:   "container became custom",
			mutate: func(_ *Config, obs *observation) { obs.containers[0].Image = "local/geth:debug" },
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "custom image",
		},
		{
			name:   "container is gone",
			mutate: func(_ *Config, obs *observation) { obs.containers = obs.containers[1:] },
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "does not exist",
		},
		{
			name:   "container is no longer managed",
			mutate: func(cfg *Config, _ *observation) { cfg.Containers = cfg.Containers[1:] },
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "not managed",
		},
		{
			name: "container and configuration moved to another tag",
			mutate: func(cfg *Config, obs *observation) {
				cfg.Containers[0].Image = "ethpandaops/geth:stable"
				obs.containers[0].Image = "ethpandaops/geth:stable"
			},
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "another tag",
		},
		{
			name:   "verb no longer allowed",
			mutate: func(cfg *Config, _ *observation) { cfg.Verbs = []protocol.Verb{protocol.VerbRoll} },
			need:   reboot,
			reason: "no longer allowed",
		},
		{
			name:   "malformed",
			mutate: func(*Config, *observation) {},
			need:   protocol.Need{ID: "manual/x", Kind: protocol.NeedImage},
			reason: "malformed",
		},
	}

	for _, tt := range drops {
		t.Run(tt.name, func(t *testing.T) {
			own := needsConfig(allVerbs...)
			obs := observed(own)
			tt.mutate(own, obs)

			kept, dropped, changed := reconcileManualNeeds(own, obs, []protocol.Need{tt.need})
			assert.True(t, changed)
			assert.Empty(t, kept)
			assert.Contains(t, dropped[tt.need.ID], tt.reason)
		})
	}

	t.Run("a stale observation drops nothing", func(t *testing.T) {
		obs := observed(cfg)
		obs.containers = nil
		obs.containersFresh = false

		manual := []protocol.Need{roll("geth", "ethpandaops/geth:master", "sha256:a")}

		kept, dropped, changed := reconcileManualNeeds(cfg, obs, manual)
		assert.False(t, changed)
		assert.Empty(t, dropped)
		assert.Equal(t, manual, kept)
	})
}

// watchedHost is a registry and image store for the watcher tests.
type watchedHost struct {
	mu         sync.Mutex
	registry   map[string]string
	local      map[string]string
	pulls      int
	resolves   int
	resolveErr error
}

func (h *watchedHost) Resolve(_ context.Context, ref string) (digest, revision string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.resolves++

	if h.resolveErr != nil {
		return "", "", h.resolveErr
	}

	return h.registry[ref], "", nil
}

func (h *watchedHost) LocalDigest(_ context.Context, ref string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.local[ref], nil
}

func (h *watchedHost) Pull(_ context.Context, ref string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.pulls++
	h.local[ref] = h.registry[ref]

	return h.local[ref], nil
}

func (h *watchedHost) Observe(context.Context) ([]protocol.Container, error) { return nil, nil }

func (h *watchedHost) Recreate(context.Context, string, string, time.Duration, bool) error {
	return errors.New("not used")
}

func (h *watchedHost) Stop(context.Context, string, time.Duration) error { return nil }

func (h *watchedHost) Start(context.Context, string) error { return nil }

func (h *watchedHost) counts() (pulls, resolves int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.pulls, h.resolves
}

func TestImageWatcher(t *testing.T) {
	const key, ref = "docker.io/library/app:latest", "app"

	host := &watchedHost{
		registry: map[string]string{ref: "sha256:1"},
		local:    map[string]string{ref: "sha256:1"},
	}

	w := newImageWatcher(quietLog(), host, host, time.Hour, time.Now, true)
	ctx := t.Context()

	_, ok := w.Check(key)
	assert.False(t, ok, "nothing is known before the first check")

	w.Track(map[string]trackedTag{key: {ref: ref, running: []string{"sha256:1"}}})

	due, _ := w.nextDue()
	require.Equal(t, key, due, "a new tag is due at once")

	w.checkTag(ctx, key)

	check, ok := w.Check(key)
	require.True(t, ok)
	assert.Equal(t, "sha256:1", check.Digest)
	assert.True(t, check.Pulled)
	assert.Empty(t, check.Error)

	due, wait := w.nextDue()
	assert.Empty(t, due, "after a check the tag rests for the poll interval")
	assert.Equal(t, maxWatchIdle, wait)

	// A container that is behind makes the watcher pull.
	host.registry[ref] = "sha256:2"

	w.RecheckAll()

	due, _ = w.nextDue()
	require.Equal(t, key, due, "recheck makes every tag due")

	w.checkTag(ctx, key)

	check, _ = w.Check(key)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.True(t, check.Pulled)

	pulls, _ := host.counts()
	assert.Equal(t, 1, pulls)

	// Nobody runs the old digest any more and the local tag was moved away
	// by hand: without a container behind, the watcher leaves it alone.
	w.Track(map[string]trackedTag{key: {ref: ref, running: []string{"sha256:2"}}})

	host.local[ref] = "sha256:other"

	w.RecheckAll()
	w.checkTag(ctx, key)

	check, _ = w.Check(key)
	assert.False(t, check.Pulled)

	pulls, _ = host.counts()
	assert.Equal(t, 1, pulls, "no pull when no container is behind")

	// A manual roll asks for the image all the same.
	ticket := w.Request(key, ref)

	_, done, gone := w.Result(key, ticket)
	assert.False(t, done)
	assert.False(t, gone)

	w.checkTag(ctx, key)

	check, done, gone = w.Result(key, ticket)
	require.True(t, done)
	assert.False(t, gone)
	assert.True(t, check.Pulled)

	pulls, _ = host.counts()
	assert.Equal(t, 2, pulls)

	// A registry error keeps the last good digest.
	host.resolveErr = errors.New("registry unreachable")

	w.RecheckAll()
	w.checkTag(ctx, key)

	check, _ = w.Check(key)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.Contains(t, check.Error, "registry unreachable")

	// A tag nobody is on any more is forgotten.
	w.Track(map[string]trackedTag{})

	_, ok = w.Check(key)
	assert.False(t, ok)

	_, _, gone = w.Result(key, ticket)
	assert.True(t, gone)
}

func TestImageWatcherPollsWithJitter(t *testing.T) {
	w := newImageWatcher(quietLog(), nil, nil, time.Minute, time.Now, true)

	lowest, highest := time.Hour, time.Duration(0)

	for range 2000 {
		d := w.jittered()
		require.GreaterOrEqual(t, d, time.Minute)
		require.LessOrEqual(t, d, 72*time.Second, "at most 20% on top")

		lowest, highest = min(lowest, d), max(highest, d)
	}

	assert.Greater(t, highest-lowest, 6*time.Second, "the interval is spread out")
}

func TestImageWatcherRunPolls(t *testing.T) {
	const key, ref = "docker.io/library/app:latest", "app"

	host := &watchedHost{
		registry: map[string]string{ref: "sha256:1"},
		local:    map[string]string{ref: "sha256:1"},
	}

	w := newImageWatcher(quietLog(), host, host, 5*time.Millisecond, time.Now, true)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		w.run(ctx)
	}()

	w.Track(map[string]trackedTag{key: {ref: ref, running: []string{"sha256:1"}}})

	require.Eventually(t, func() bool {
		_, resolves := host.counts()

		return resolves >= 4
	}, 5*time.Second, time.Millisecond, "the watcher keeps polling at its interval")

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the watcher did not stop")
	}
}

// On a host without the roll verb the watcher looks, for the report, and
// pulls nothing, whoever asks.
func TestImageWatcherDoesNotPullWhenTheHostNeverRolls(t *testing.T) {
	const key, ref = "docker.io/library/app:latest", "app"

	host := &watchedHost{
		registry: map[string]string{ref: "sha256:2"},
		local:    map[string]string{ref: "sha256:1"},
	}

	w := newImageWatcher(quietLog(), host, host, time.Hour, time.Now, false)
	ctx := t.Context()

	w.Track(map[string]trackedTag{key: {ref: ref, running: []string{"sha256:1"}}})
	w.checkTag(ctx, key)

	check, ok := w.Check(key)
	require.True(t, ok)
	assert.Equal(t, "sha256:2", check.Digest)
	assert.False(t, check.Pulled)
	assert.Empty(t, check.Error)

	ticket := w.Request(key, ref)
	w.checkTag(ctx, key)

	check, done, _ := w.Result(key, ticket)
	require.True(t, done)
	assert.False(t, check.Pulled)

	pulls, resolves := host.counts()
	assert.Equal(t, 0, pulls)
	assert.Equal(t, 2, resolves)
	assert.Equal(t, "sha256:1", host.local[ref], "the local tag is left where it was")
}
