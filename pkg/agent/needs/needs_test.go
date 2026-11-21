package needs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func needsConfig(verbs ...protocol.Verb) *config.Config {
	return &config.Config{
		Verbs:    verbs,
		Groups:   map[string]config.Group{"execution": {Client: "geth"}},
		Packages: config.Packages{Blocked: []string{"docker-ce", "containerd.io"}},
	}
}

var allVerbs = []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot}

// observed builds an Observation in which geth has a pulled new image,
// teku is up to date, docker-ce and an unblocked package can be upgraded
// and both reboot reasons apply.
func observed(cfg *config.Config) *Observation {
	return observedWith(cfg, nil)
}

// observedWith is observed on a host whose overrides file says set.
func observedWith(cfg *config.Config, set *overrides.Set) *Observation {
	containers := []protocol.Container{
		{
			Name: "geth", Image: "ethpandaops/geth:master", Digest: "sha256:a", State: protocol.StateRunning,
			Group: "execution", Kind: protocol.KindEL,
		},
		{
			Name: "teku", Image: "consensys/teku:develop", Digest: "sha256:t", State: protocol.StateRunning,
			Group: "consensus", Kind: protocol.KindCL,
		},
		{Name: "dora", Image: "ethpandaops/dora:latest", Digest: "sha256:d", State: protocol.StateRunning},
	}

	checks := map[string]protocol.RegistryCheck{
		"docker.io/ethpandaops/geth:master": {Digest: "sha256:b", Revision: "r2", Pulled: true},
		"docker.io/consensys/teku:develop":  {Digest: "sha256:t", Pulled: true},
		"docker.io/ethpandaops/dora:latest": {Digest: "sha256:e", Pulled: true},
	}

	check := func(key string) (protocol.RegistryCheck, bool) {
		c, ok := checks[key]

		return c, ok
	}

	_, targets := Enrich(cfg, containers, set, check)

	return &Observation{
		Containers:      containers,
		ContainersFresh: true,
		Targets:         targets,
		Check:           check,
		Sys: &hostapi.SystemStatus{
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

// notPulled makes the Observation say that the new geth image is not local.
func notPulled(obs *Observation) {
	obs.Containers[0].Registry.Pulled = false

	inner := obs.Check
	obs.Check = func(key string) (protocol.RegistryCheck, bool) {
		rc, ok := inner(key)
		rc.Pulled = false

		return rc, ok
	}
}

// A need whose image is not local yet says what it waits for: the image is
// pulled in a pull window first.
func TestNeedsSayWhatTheyWaitFor(t *testing.T) {
	cfg := needsConfig(protocol.VerbRoll)
	obs := observed(cfg)

	got := Compute(cfg, obs, nil)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Unpulled, "the image is local")

	notPulled(obs)

	got = Compute(cfg, obs, nil)
	require.Len(t, got, 1)
	assert.Equal(t, []protocol.PullImage{{Ref: "ethpandaops/geth:master", Digest: "sha256:b"}}, got[0].Unpulled)
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
		mutate func(obs *Observation)
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
			mutate: func(obs *Observation) { obs.Hold.Local = true },
			manual: []protocol.Need{manualRoll, manualReboot},
			want:   []string{},
		},
		{
			name:   "controller hold too",
			verbs:  allVerbs,
			mutate: func(obs *Observation) { obs.Hold.Controller = true },
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
			mutate: notPulled,
			want:   []string{imageID},
		},
		{
			name:   "registry never answered",
			verbs:  []protocol.Verb{protocol.VerbRoll},
			mutate: func(obs *Observation) { obs.Containers[0].Registry = nil },
			want:   []string{},
		},
		{
			name:  "kernel only",
			verbs: []protocol.Verb{protocol.VerbReboot},
			mutate: func(obs *Observation) {
				obs.Sys.OS.RebootRequired = false
			},
			want: []string{protocol.RebootNeedID},
		},
		{
			name:  "no reboot reason",
			verbs: []protocol.Verb{protocol.VerbReboot},
			mutate: func(obs *Observation) {
				obs.Sys.OS.RebootRequired = false
				obs.Sys.OS.NewestKernel = obs.Sys.OS.RunningKernel
			},
			want: []string{},
		},
		{
			name:   "system state unknown",
			verbs:  allVerbs,
			mutate: func(obs *Observation) { obs.Sys = nil },
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

			got := Compute(cfg, obs, tt.manual)
			require.NotNil(t, got, "no needs is an empty list, not null")
			assert.Equal(t, tt.want, ids(got))
		})
	}

	// The content of the observed needs.
	cfg := needsConfig(allVerbs...)
	got := Compute(cfg, observed(cfg), nil)
	require.Len(t, got, 3)
	assert.Equal(t, &protocol.ImageNeed{
		Container: "geth", Group: "execution", Client: "geth", Image: "ethpandaops/geth:master",
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

		kept, dropped, changed := Reconcile(cfg, observed(cfg), manual)
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

		kept, dropped, changed := Reconcile(cfg, observed(cfg), manual)
		assert.False(t, changed)
		assert.Empty(t, dropped)
		assert.Equal(t, manual, kept)
	})

	t.Run("a tag that is not pulled yet is followed: the need waits for the pull", func(t *testing.T) {
		obs := observed(cfg)
		obs.Containers[0].Registry.Pulled = false
		obs.Containers[0].Digest = "sha256:old"

		kept, _, changed := Reconcile(cfg, obs,
			[]protocol.Need{roll("geth", "ethpandaops/geth:master", "sha256:a")})
		assert.True(t, changed)
		assert.Equal(t, "sha256:b", kept[0].Image.ToDigest)
	})

	drops := []struct {
		name   string
		mutate func(cfg *config.Config, obs *Observation)
		need   protocol.Need
		reason string
	}{
		{
			name:   "container is gone",
			mutate: func(_ *config.Config, obs *Observation) { obs.Containers = obs.Containers[1:] },
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "does not exist",
		},
		{
			name:   "container is no longer managed",
			mutate: func(_ *config.Config, obs *Observation) { obs.Containers[0].Managed = false },
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "not managed",
		},
		{
			name: "container moved to another tag",
			mutate: func(_ *config.Config, obs *Observation) {
				obs.Containers[0].Image = "ethpandaops/geth:stable"
			},
			need:   roll("geth", "ethpandaops/geth:master", "sha256:a"),
			reason: "another tag",
		},
		{
			name:   "verb no longer allowed",
			mutate: func(cfg *config.Config, _ *Observation) { cfg.Verbs = []protocol.Verb{protocol.VerbRoll} },
			need:   reboot,
			reason: "no longer allowed",
		},
		{
			name:   "malformed",
			mutate: func(*config.Config, *Observation) {},
			need:   protocol.Need{ID: "manual/x", Kind: protocol.NeedImage},
			reason: "malformed",
		},
	}

	for _, tt := range drops {
		t.Run(tt.name, func(t *testing.T) {
			own := needsConfig(allVerbs...)
			obs := observed(own)
			tt.mutate(own, obs)

			kept, dropped, changed := Reconcile(own, obs, []protocol.Need{tt.need})
			assert.True(t, changed)
			assert.Empty(t, kept)
			assert.Contains(t, dropped[tt.need.ID], tt.reason)
		})
	}

	t.Run("a stale Observation drops nothing", func(t *testing.T) {
		obs := observed(cfg)
		obs.Containers = nil
		obs.ContainersFresh = false

		manual := []protocol.Need{roll("geth", "ethpandaops/geth:master", "sha256:a")}

		kept, dropped, changed := Reconcile(cfg, obs, manual)
		assert.False(t, changed)
		assert.Empty(t, dropped)
		assert.Equal(t, manual, kept)
	})
}

// A group's impact is what agent.yaml says, or node when it has a container
// of the node and independent when it has none.
func TestGroupImpacts(t *testing.T) {
	cfg := &config.Config{Groups: map[string]config.Group{"tools": {Impact: protocol.ImpactSlot}}}
	list := []protocol.Container{
		{Name: "beacon", Managed: true, Group: "consensus", Kind: protocol.KindCL},
		{Name: "exporter", Managed: true, Group: "consensus", Kind: protocol.KindOther},
		{Name: "nginx", Managed: true, Group: "proxy", Kind: protocol.KindOther},
		{Name: "dora", Managed: true, Group: "tools", Kind: protocol.KindOther},
		{Name: "cadvisor", Managed: true, Group: protocol.DefaultGroup, Kind: protocol.KindOther},
		{Name: "debug"},
	}

	assert.Equal(t, map[string]protocol.Impact{
		"consensus": protocol.ImpactNode, "proxy": protocol.ImpactIndependent, "tools": protocol.ImpactSlot,
		protocol.DefaultGroup: protocol.ImpactIndependent,
	}, groupImpacts(cfg, list))
}

// A container's group is its label, else its entry in the name map of
// agent.yaml, else the default group; the label none and the default group's
// exclude list leave a container out of everything.
func TestEnrichResolvesGroups(t *testing.T) {
	cfg := &config.Config{
		Containers: map[string]config.MappedContainer{
			"execution": {Group: "execution", Kind: protocol.KindEL},
			"beacon":    {Group: "consensus", Kind: protocol.KindCL},
			"skipped":   {Group: "consensus", Kind: protocol.KindCL},
		},
		DefaultGroup: config.DefaultGroup{Exclude: []string{"tmp-*", "debug"}},
	}
	observed := func() []protocol.Container {
		return []protocol.Container{
			{Name: "execution", Image: "geth:1"},
			{Name: "beacon", Image: "lighthouse:1", Group: "consensus-a", Kind: protocol.KindCL},
			{Name: "skipped", Image: "lighthouse:1", Group: protocol.GroupNone},
			{Name: "cadvisor", Image: "cadvisor:1", Command: []string{"-v"}},
			{Name: "pinned", Image: "cadvisor@sha256:aa"},
			{Name: "linked", Image: "nginx:1", Linked: true},
			{Name: "tmp-1", Image: "busybox:1"},
			{Name: "debug", Image: "busybox:1"},
		}
	}
	check := func(string) (protocol.RegistryCheck, bool) {
		return protocol.RegistryCheck{Digest: "sha256:new"}, true
	}

	list := observed()
	tags, targets := Enrich(cfg, list, overrides.NewSet(), check)

	type got struct {
		managed    bool
		group      string
		kind       protocol.ContainerKind
		unlabelled bool
		notRolled  bool
	}

	want := map[string]got{
		"execution": {true, "execution", protocol.KindEL, true, false},
		"beacon":    {true, "consensus-a", protocol.KindCL, false, false},
		"skipped":   {},
		"cadvisor":  {true, protocol.DefaultGroup, protocol.KindOther, false, true},
		"pinned":    {true, protocol.DefaultGroup, protocol.KindOther, false, true},
		"linked":    {true, protocol.DefaultGroup, protocol.KindOther, false, true},
		"tmp-1":     {},
		"debug":     {},
	}

	for i := range list {
		c := &list[i]
		assert.Equal(t, want[c.Name], got{c.Managed, c.Group, c.Kind, c.Unlabelled, c.NotRolled != ""}, c.Name)
	}

	cadvisor := list[3]
	assert.Equal(t, protocol.ImpactIndependent, cadvisor.Impact)
	assert.Equal(t, &protocol.RegistryCheck{Digest: "sha256:new"}, cadvisor.Registry, "its updates are reported")
	assert.Contains(t, tags, imageref.Normalize("cadvisor:1"))
	assert.NotContains(t, targets, "cadvisor", "the default group knows no overrides")
	assert.Contains(t, targets, "execution")

	_, refused := Rollable(&cadvisor, "cadvisor")
	assert.True(t, refused, "not rolled while defaultGroup.roll is off")
	assert.Empty(t, Compute(cfg, &Observation{Containers: list}, nil), "no need while roll is off")

	cfg.DefaultGroup.Roll = true
	cfg.Verbs = []protocol.Verb{protocol.VerbRoll}
	list = observed()
	Enrich(cfg, list, overrides.NewSet(), check)

	assert.Empty(t, list[3].NotRolled, "rolled with defaultGroup.roll")
	assert.NotEmpty(t, list[4].NotRolled, "a digest is never rolled")
	assert.NotEmpty(t, list[5].NotRolled, "links are never rolled")

	got2 := Compute(cfg, &Observation{Containers: list[3:6]}, nil)
	require.Len(t, got2, 1)
	assert.Equal(t, "cadvisor", got2[0].Image.Container)
	assert.Equal(t, protocol.DefaultGroup, got2[0].Image.Group)
}

// Containers are managed by their group label; the group's entry in
// agent.yaml gives client, owners and what may be overridden, and a group
// without one is admin-only.
func TestEnrichContainersByLabels(t *testing.T) {
	cfg := &config.Config{Groups: map[string]config.Group{
		"consensus": {Client: "lighthouse", Owners: []string{"lighthouse"}, Overrides: []string{"image"}},
	}}
	set := overrides.NewSet()
	set.Deployed["beacon"] = overrides.Deployed{Image: "sigp/lighthouse:v2"}

	list := []protocol.Container{
		{Name: "beacon", Image: "sigp/lighthouse:v1", Group: "consensus", Kind: protocol.KindCL},
		{Name: "nginx", Image: "nginx:1", Group: "proxy", Kind: protocol.KindOther, Command: []string{"-g"}},
		{Name: "debug", Image: "busybox:1", Command: []string{"sh"}, Group: protocol.GroupNone},
	}

	_, targets := Enrich(cfg, list, set, func(string) (protocol.RegistryCheck, bool) {
		return protocol.RegistryCheck{}, false
	})

	beacon, nginx, debug := list[0], list[1], list[2]
	assert.True(t, beacon.Managed)
	assert.Equal(t, "lighthouse", beacon.Client)
	assert.Equal(t, []string{"lighthouse"}, beacon.Owners)
	assert.Equal(t, &protocol.Overridable{Image: true}, beacon.Overridable)
	assert.Equal(t, "sigp/lighthouse:v2", beacon.ConfiguredImage, "what the deployed block says")
	assert.True(t, targets["beacon"].Spec, "the block names another image: a soft deploy")

	assert.True(t, nginx.Managed)
	assert.Empty(t, nginx.Client)
	assert.Nil(t, nginx.Overridable, "a group without an entry is tooling")
	assert.Equal(t, protocol.ImpactIndependent, nginx.Impact)
	assert.Equal(t, "nginx:1", nginx.ConfiguredImage, "without a deployed entry: what it runs")
	assert.True(t, targets["nginx"].InSync)

	assert.False(t, debug.Managed)
	assert.Nil(t, debug.Command, "the command line of an unmanaged container is not reported")
	assert.NotContains(t, targets, "debug")
}
