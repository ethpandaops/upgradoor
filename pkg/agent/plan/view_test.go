package plan

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// A group's impact is its configured one, else node with an el, cl or vc, else independent.
func TestGroupImpacts(t *testing.T) {
	p := &Policy{Groups: map[string]Group{"tools": {Impact: protocol.ImpactSlot}}}
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
	}, groupImpacts(p, list))
}

// The group is the label, else the name map, else default; label none or exclude: unmanaged.
func TestClassifyResolvesGroups(t *testing.T) {
	p := &Policy{
		Containers: map[string]MappedContainer{
			"execution": {Group: "execution", Kind: protocol.KindEL},
			"beacon":    {Group: "consensus", Kind: protocol.KindCL},
			"skipped":   {Group: "consensus", Kind: protocol.KindCL},
		},
		DefaultGroup: DefaultGroup{Exclude: []string{"tmp-*", "debug"}},
	}
	facts := &Facts{Fresh: true, Containers: []protocol.Container{
		{Name: "execution", Image: "geth:1"},
		{Name: "beacon", Image: "lighthouse:1", Group: "consensus-a", Kind: protocol.KindCL},
		{Name: "skipped", Image: "lighthouse:skipped", Group: protocol.GroupNone},
		{Name: "cadvisor", Image: "cadvisor:1", Command: []string{"-v"}},
		{Name: "pinned", Image: "cadvisor@sha256:aa"},
		{Name: "linked", Image: "nginx:1", Linked: true},
		{Name: "tmp-1", Image: "busybox:1"},
		{Name: "debug", Image: "busybox:1"},
	}}
	checks := map[string]protocol.RegistryCheck{}

	for _, c := range facts.Containers {
		checks[imageref.Normalize(c.Image)] = protocol.RegistryCheck{Digest: "sha256:new"}
	}

	view := Classify(p, facts, known(), checks, testNow)

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

	for i := range view.Containers {
		c := &view.Containers[i]
		assert.Equal(t, want[c.Name], got{c.Managed, c.Group, c.Kind, c.Unlabelled, c.NotRolled != ""}, c.Name)
	}

	assert.False(t, facts.Containers[0].Managed, "the facts are not changed")
	assert.Empty(t, facts.Containers[0].Group)

	cadvisor := view.Containers[3]
	assert.Equal(t, protocol.ImpactIndependent, cadvisor.Impact)
	assert.Equal(t, &protocol.RegistryCheck{Digest: "sha256:new"}, cadvisor.Registry, "its updates are reported")
	assert.Equal(t, "cadvisor:1", view.Tags[imageref.Normalize("cadvisor:1")])
	assert.NotContains(t, view.Tags, imageref.Normalize("cadvisor@sha256:aa"), "a digest is not followed")
	assert.NotContains(t, view.Tags, imageref.Normalize("lighthouse:skipped"), "label none: not followed")
	assert.NotContains(t, view.Tags, imageref.Normalize("busybox:1"), "defaultGroup.exclude: not followed")
	assert.Len(t, view.Tags, 4, "geth, lighthouse, cadvisor and nginx")
	assert.NotContains(t, view.Targets, "cadvisor", "the default group knows no overrides")
	assert.Contains(t, view.Targets, "execution")

	_, refused := rollable(&cadvisor, "cadvisor")
	assert.True(t, refused, "not rolled while defaultGroup.roll is off")

	p.Verbs = []protocol.Verb{protocol.VerbRoll}
	view.Containers = view.Containers[3:6]
	assert.Empty(t, Needs(p, &view, nil), "no need while roll is off")

	p.DefaultGroup.Roll = true
	view = Classify(p, facts, known(), checks, testNow)

	assert.Empty(t, view.Containers[3].NotRolled, "rolled with defaultGroup.roll")
	assert.NotEmpty(t, view.Containers[4].NotRolled, "a digest is never rolled")
	assert.NotEmpty(t, view.Containers[5].NotRolled, "links are never rolled")

	view.Containers = view.Containers[3:6]
	got2 := Needs(p, &view, nil)
	require.Len(t, got2, 1)
	assert.Equal(t, "cadvisor", got2[0].Image.Container)
	assert.Equal(t, protocol.DefaultGroup, got2[0].Image.Group)
}

// A group's entry gives client, owners and overridable parts; a group without one has none.
func TestClassifyContainersByLabels(t *testing.T) {
	p := &Policy{Groups: map[string]Group{
		"consensus": {Client: "lighthouse", Owners: []string{"lighthouse"}, Overrides: []string{"image"}},
	}}
	files := known()
	files.Deployed = map[string]Deployed{"beacon": {Image: "sigp/lighthouse:v2"}}

	facts := &Facts{Fresh: true, Containers: []protocol.Container{
		{Name: "beacon", Image: "sigp/lighthouse:v1", Group: "consensus", Kind: protocol.KindCL},
		{Name: "nginx", Image: "nginx:1", Group: "proxy", Kind: protocol.KindOther, Command: []string{"-g"}},
		{Name: "debug", Image: "busybox:1", Command: []string{"sh"}, Group: protocol.GroupNone},
	}}

	view := Classify(p, facts, files, nil, testNow)

	beacon, nginx, debug := view.Containers[0], view.Containers[1], view.Containers[2]
	assert.True(t, beacon.Managed)
	assert.Equal(t, "lighthouse", beacon.Client)
	assert.Equal(t, []string{"lighthouse"}, beacon.Owners)
	assert.Equal(t, &protocol.Overridable{Image: true}, beacon.Overridable)
	assert.Equal(t, "sigp/lighthouse:v2", beacon.ConfiguredImage, "what the deployed block says")
	assert.True(t, view.Targets["beacon"].Spec, "the block names another image: a soft deploy")
	assert.Equal(t, "sigp/lighthouse:v2", view.Tags[imageref.Normalize("sigp/lighthouse:v2")],
		"the tag of the target is followed, though no container runs it")

	assert.True(t, nginx.Managed)
	assert.Empty(t, nginx.Client)
	assert.Nil(t, nginx.Overridable, "a group without an entry is tooling")
	assert.Equal(t, protocol.ImpactIndependent, nginx.Impact)
	assert.Equal(t, "nginx:1", nginx.ConfiguredImage, "without a deployed entry: what it runs")
	assert.True(t, view.Targets["nginx"].InSync)

	assert.False(t, debug.Managed)
	assert.Nil(t, debug.Command, "the command line of an unmanaged container is not reported")
	assert.NotContains(t, view.Targets, "debug")
	assert.Equal(t, []string{"sh"}, facts.Containers[2].Command, "the facts keep it")

	// Nothing written through the view reaches the facts.
	view.Containers[1].Command[0] = "changed"
	assert.Equal(t, []string{"-g"}, facts.Containers[1].Command)

	facts.Containers[0].Base = &protocol.OverrideBase{Command: []string{"--a"}, HasCommand: true}
	view = Classify(p, facts, files, nil, testNow)
	view.Containers[0].Base.Command[0] = "changed"
	assert.Equal(t, []string{"--a"}, facts.Containers[0].Base.Command)
}

// An expired entry is gone; an unreadable overrides file changes nothing and holds the host.
func TestClassifyOverrides(t *testing.T) {
	p := &Policy{Groups: map[string]Group{"execution": {Overrides: []string{"image", "flags"}}}}
	past, future := testNow.Add(-time.Minute), testNow.Add(time.Hour)
	facts := &Facts{Fresh: true, Containers: []protocol.Container{{
		Name: "geth", Image: "ethpandaops/geth:fix", Group: "execution", Kind: protocol.KindEL,
		Base: &protocol.OverrideBase{Image: "ethpandaops/geth:master"},
	}}}

	files := known()
	files.Entries = map[string]Entry{"geth": {Image: "ethpandaops/geth:fix", By: "sam", Until: &future}}
	view := Classify(p, facts, files, nil, testNow)
	assert.Equal(t, &protocol.ContainerOverride{
		Image: "ethpandaops/geth:fix", By: "sam", Until: &future, Applied: true,
	}, view.Containers[0].Override)
	assert.Equal(t, "ethpandaops/geth:master", view.Containers[0].ConfiguredImage, "the base label")

	files.Entries = map[string]Entry{"geth": {Image: "ethpandaops/geth:fix", By: "sam", Until: &past}}
	view = Classify(p, facts, files, nil, testNow)
	assert.True(t, view.Targets["geth"].Restore, "an expired entry is gone")
	assert.Equal(t, &protocol.ContainerOverride{Restoring: true, Image: "ethpandaops/geth:fix"},
		view.Containers[0].Override)

	broken := &Overrides{Err: "yaml: line 3"}
	view = Classify(p, facts, broken, nil, testNow)
	assert.True(t, view.Targets["geth"].InSync, "nothing is put back")
	assert.Contains(t, view.Targets["geth"].Err, "the overrides file cannot be read (yaml: line 3)")
	assert.Equal(t, "yaml: line 3", view.OverridesError)
	assert.True(t, view.Hold.Local)
}

func TestHoldState(t *testing.T) {
	past, future := testNow.Add(-time.Minute), testNow.Add(time.Hour)

	for _, tt := range []struct {
		name  string
		files *Overrides
		want  protocol.Hold
	}{
		{"no hold", known(), protocol.Hold{}},
		{
			"unreadable file holds the host", &Overrides{Err: "bad\nfile"},
			protocol.Hold{Local: true, LocalReason: "the overrides file cannot be read: bad"},
		},
		{
			"local", &Overrides{Known: true, Holds: map[string]Hold{HoldLocal: {Reason: "debugging"}}},
			protocol.Hold{Local: true, LocalReason: "debugging"},
		},
		{
			"controller", &Overrides{Known: true, Holds: map[string]Hold{HoldController: {Until: &future}}},
			protocol.Hold{Controller: true},
		},
		{
			"expired holds", &Overrides{Known: true, Holds: map[string]Hold{
				HoldLocal: {Until: &past}, HoldController: {Until: &testNow},
			}},
			protocol.Hold{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HoldState(tt.files, testNow))
		})
	}
}

func TestHoldReason(t *testing.T) {
	assert.Equal(t, "host is on local hold: x", HoldReason(protocol.Hold{Local: true, LocalReason: "x", Controller: true}))
	assert.Equal(t, "host is on local hold", HoldReason(protocol.Hold{Local: true}))
	assert.Equal(t, "host is on controller hold", HoldReason(protocol.Hold{Controller: true}))
}

// init-overrides writes what every managed container was deployed as.
func TestDeployment(t *testing.T) {
	view := &View{Containers: []protocol.Container{
		{Name: "geth", Managed: true, ConfiguredImage: "ethpandaops/geth:master", Command: []string{"--x", "--pprof"},
			Base: &protocol.OverrideBase{Command: []string{"--x"}, HasCommand: true}},
		{Name: "dora", Managed: true, ConfiguredImage: "ethpandaops/dora:latest"},
		{Name: "debug", Image: "busybox:1"},
	}}

	assert.Equal(t, map[string]Deployed{
		"geth": {Image: "ethpandaops/geth:master", Command: []string{"--x"}, HasCommand: true},
		"dora": {Image: "ethpandaops/dora:latest", Command: []string{}, HasCommand: true},
	}, Deployment(view))
}

// A deploy hands a container to the agent only when the agent rolls it to its target.
func TestDeployState(t *testing.T) {
	p := &Policy{Verbs: []protocol.Verb{protocol.VerbRoll}}
	facts := &Facts{Fresh: true, Containers: []protocol.Container{
		{Name: "geth", Image: "geth:1", Digest: "sha256:a", Group: "el", Command: []string{"--x"}},
		{Name: "teku", Image: "teku:1", Digest: "sha256:a", Group: "cl"},
		{Name: "besu", Image: "besu:1", Digest: "sha256:old", Group: "el"},
		{Name: "nginx", Image: "nginx:1", Group: "proxy", Linked: true},
		{Name: "cadvisor", Image: "cadvisor:1"},
		{Name: "dora", Image: "dora:1", Group: protocol.GroupNone},
	}}
	files := &Overrides{Known: true, Deployed: map[string]Deployed{
		"geth": {Image: "geth:1", Command: []string{"--y"}, HasCommand: true},
		"teku": {Image: "teku:1"},
	}}
	checks := map[string]protocol.RegistryCheck{imageref.Normalize("besu:1"): {Digest: "sha256:new"}}
	view := Classify(p, facts, files, checks, testNow)

	for name, want := range map[string][3]bool{
		"geth":     {true, true, true},
		"teku":     {true, true, false},
		"besu":     {true, true, true},
		"nginx":    {true, false, false},
		"cadvisor": {true, false, false},
		"dora":     {true, false, false},
		"missing":  {false, false, false},
	} {
		exists, managed, differs := DeployState(p, &view, name)
		assert.Equal(t, want, [3]bool{exists, managed, differs}, name)
	}

	exists, managed, _ := DeployState(&Policy{}, &view, "geth")
	assert.Equal(t, [2]bool{true, false}, [2]bool{exists, managed}, "without the roll verb")
}
