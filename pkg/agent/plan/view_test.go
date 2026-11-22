package plan

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The labels decide: any kind but none manages a container with impact node,
// an invalid label or none leaves it out, and without a kind label
// defaultPolicy decides (impact independent when rolled).
func TestClassifyResolvesLabels(t *testing.T) {
	p := &Policy{DefaultPolicy: DefaultIgnore}
	facts := &Facts{Fresh: true, Containers: []protocol.Container{
		{Name: "execution", Image: "geth:1", Kind: protocol.KindEL, Team: "geth"},
		{Name: "snooper", Image: "snooper:1", Kind: "snooper-el"},
		{Name: "skipped", Image: "lighthouse:skipped", Kind: protocol.KindNone, Team: "lighthouse"},
		{Name: "bad-kind", Image: "busybox:1", Kind: "Snooper"},
		{Name: "bad-team", Image: "busybox:1", Kind: "el", Team: "Geth"},
		{Name: "cadvisor", Image: "cadvisor:1", Command: []string{"-v"}, Team: "ignored"},
		{Name: "pinned", Image: "cadvisor@sha256:aa"},
		{Name: "linked", Image: "nginx:1", Linked: true},
	}}
	checks := map[string]protocol.RegistryCheck{}

	for _, c := range facts.Containers {
		checks[imageref.Normalize(c.Image)] = protocol.RegistryCheck{Digest: "sha256:new"}
	}

	type got struct {
		managed   bool
		kind      protocol.ContainerKind
		team      string
		impact    protocol.Impact
		notRolled bool
	}

	labelled := map[string]got{
		"execution": {true, protocol.KindEL, "geth", protocol.ImpactNode, false},
		"snooper":   {true, "snooper-el", "", protocol.ImpactNode, false},
		"skipped":   {},
		"bad-kind":  {},
		"bad-team":  {},
	}
	unlabelled := map[string]got{
		"cadvisor": {true, "", "", protocol.ImpactIndependent, false},
		"pinned":   {true, "", "", protocol.ImpactIndependent, true},
		"linked":   {true, "", "", protocol.ImpactIndependent, true},
	}

	view := Classify(p, facts, known(), checks, testNow)

	for i := range view.Containers {
		c := &view.Containers[i]
		want, ok := labelled[c.Name]
		if !ok {
			want = got{}
		}

		assert.Equal(t, want, got{c.Managed, c.Kind, c.Team, c.Impact, c.NotRolled != ""}, c.Name)
	}

	assert.False(t, facts.Containers[0].Managed, "the facts are not changed")
	assert.Len(t, view.Tags, 2, "geth and snooper")
	assert.Contains(t, view.Targets, "execution")
	assert.Contains(t, view.Targets, "snooper")

	p.DefaultPolicy = DefaultRoll
	view = Classify(p, facts, known(), checks, testNow)

	for i := range view.Containers {
		c := &view.Containers[i]
		want, ok := labelled[c.Name]
		if !ok {
			want = unlabelled[c.Name]
		}

		assert.Equal(t, want, got{c.Managed, c.Kind, c.Team, c.Impact, c.NotRolled != ""}, c.Name)
	}

	cadvisor := view.Container("cadvisor")
	assert.Equal(t, &protocol.RegistryCheck{Digest: "sha256:new"}, cadvisor.Registry, "its updates are reported")
	assert.NotContains(t, view.Tags, imageref.Normalize("cadvisor@sha256:aa"), "a digest is not followed")
	assert.NotContains(t, view.Tags, imageref.Normalize("lighthouse:skipped"), "kind none: not followed")
	assert.NotContains(t, view.Targets, "cadvisor", "an unlabelled container has no target")
	assert.False(t, cadvisor.Overridable)

	p.Verbs = []protocol.Verb{protocol.VerbRoll}
	got2 := Needs(p, &view, nil)
	require.Len(t, got2, 3, "geth, the snooper and cadvisor; never a digest or links")
	assert.Equal(t, "cadvisor", got2[0].Image.Container)
	assert.Empty(t, got2[0].Image.Kind)
	assert.Equal(t, protocol.KindEL, got2[1].Image.Kind)
	assert.Equal(t, "geth", got2[1].Image.Team)
	assert.Equal(t, protocol.ContainerKind("snooper-el"), got2[2].Image.Kind)
}

// A labelled container has a target from managed.yaml; only one with a team
// is overridable, and only with overrides enabled.
func TestClassifyContainersByLabels(t *testing.T) {
	p := &Policy{}
	files := known()
	files.Deployed = map[string]Deployed{"beacon": {Image: "sigp/lighthouse:v2"}}

	facts := &Facts{Fresh: true, Containers: []protocol.Container{
		{Name: "beacon", Image: "sigp/lighthouse:v1", Kind: protocol.KindCL, Team: "lighthouse"},
		{Name: "snooper", Image: "snooper:1", Kind: "snooper-cl", Command: []string{"-g"}},
		{Name: "debug", Image: "busybox:1", Command: []string{"sh"}, Kind: protocol.KindNone},
	}}

	view := Classify(p, facts, files, nil, testNow)

	beacon, snooper, debug := view.Containers[0], view.Containers[1], view.Containers[2]
	assert.True(t, beacon.Managed)
	assert.Equal(t, "lighthouse", beacon.Team)
	assert.True(t, beacon.Overridable)
	assert.Equal(t, "sigp/lighthouse:v2", beacon.ConfiguredImage, "what the deployed block says")
	assert.True(t, view.Targets["beacon"].Spec, "the block names another image: a soft deploy")
	assert.Equal(t, "sigp/lighthouse:v2", view.Tags[imageref.Normalize("sigp/lighthouse:v2")],
		"the tag of the target is followed, though no container runs it")

	assert.True(t, snooper.Managed)
	assert.Empty(t, snooper.Team)
	assert.False(t, snooper.Overridable, "no team: not overridable")
	assert.Equal(t, protocol.ImpactNode, snooper.Impact)
	assert.Equal(t, "snooper:1", snooper.ConfiguredImage, "without a deployed entry: what it runs")
	assert.True(t, view.Targets["snooper"].InSync)

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

	disabled := *files
	disabled.Disabled = true
	view = Classify(p, facts, &disabled, nil, testNow)
	assert.False(t, view.Container("beacon").Overridable, "overrides disabled")
}

// An expired entry is gone; an unreadable overrides file changes nothing and holds the host.
func TestClassifyOverrides(t *testing.T) {
	p := &Policy{}
	past, future := testNow.Add(-time.Minute), testNow.Add(time.Hour)
	facts := &Facts{Fresh: true, Containers: []protocol.Container{{
		Name: "geth", Image: "ethpandaops/geth:fix", Kind: protocol.KindEL, Team: "geth",
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

	broken := &Overrides{Err: "the overrides file cannot be read: yaml: line 3"}
	view = Classify(p, facts, broken, nil, testNow)
	assert.True(t, view.Targets["geth"].InSync, "nothing is put back")
	assert.Equal(t, "the overrides file cannot be read: yaml: line 3; nothing is changed", view.Targets["geth"].Err)
	assert.Equal(t, "the overrides file cannot be read: yaml: line 3", view.OverridesError)
	assert.True(t, view.Hold.Local)
}

// staffHold is the hold state of a host that staff holds.
var staffHold = protocol.Hold{Holds: []protocol.HostHold{{Holder: protocol.HolderStaff}}}

func TestHoldState(t *testing.T) {
	past, future := testNow.Add(-time.Minute), testNow.Add(time.Hour)

	for _, tt := range []struct {
		name  string
		files *Overrides
		want  protocol.Hold
	}{
		{"no hold", known(), protocol.Hold{}},
		{
			"unreadable file holds the host", &Overrides{Err: "managed.yaml cannot be read: bad\nfile"},
			protocol.Hold{
				Local: true, LocalReason: "managed.yaml cannot be read: bad",
				Holds: []protocol.HostHold{{Holder: protocol.HolderHost, Reason: "managed.yaml cannot be read: bad"}},
			},
		},
		{
			"local", &Overrides{Known: true, Holds: map[string]Hold{protocol.HolderHost: {Reason: "debugging"}}},
			protocol.Hold{
				Local: true, LocalReason: "debugging",
				Holds: []protocol.HostHold{{Holder: protocol.HolderHost, Reason: "debugging"}},
			},
		},
		{
			"staff's", &Overrides{Known: true, Holds: map[string]Hold{protocol.HolderStaff: {Until: &future}}},
			staffHold,
		},
		{
			"every holder, in order", &Overrides{Known: true, Holds: map[string]Hold{
				"lighthouse": {By: "lh", At: testNow}, protocol.HolderStaff: {By: "sam", Reason: "fork"},
				"geth": {}, protocol.HolderHost: {By: "pk", Reason: "oom"},
			}},
			protocol.Hold{Local: true, LocalReason: "oom", Holds: []protocol.HostHold{
				{Holder: protocol.HolderHost, By: "pk", Reason: "oom"},
				{Holder: protocol.HolderStaff, By: "sam", Reason: "fork"},
				{Holder: "geth"},
				{Holder: "lighthouse", By: "lh", At: testNow},
			}},
		},
		{
			"expired holds", &Overrides{Known: true, Holds: map[string]Hold{
				protocol.HolderHost: {Until: &past}, protocol.HolderStaff: {Until: &testNow},
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
	assert.Equal(t, "host is on local hold: x", HoldReason(protocol.Hold{
		Local: true, LocalReason: "x",
		Holds: []protocol.HostHold{{Holder: protocol.HolderHost}, {Holder: protocol.HolderStaff}},
	}))
	assert.Equal(t, "host is on local hold", HoldReason(protocol.Hold{Local: true}))
	assert.Equal(t, "host is on hold by staff", HoldReason(staffHold))
	assert.Equal(t, "host is on hold by staff, geth", HoldReason(protocol.Hold{
		Holds: []protocol.HostHold{{Holder: protocol.HolderStaff}, {Holder: "geth"}},
	}))
}

// The overrides file has a key for each overridable container, showing
// managed.yaml's default, else what the container was created with.
func TestOverrideKeys(t *testing.T) {
	p := &Policy{DefaultPolicy: DefaultRoll}
	facts := &Facts{Fresh: true, Containers: []protocol.Container{
		{Name: "geth", Image: "geth:fix", Kind: "el", Team: "geth", Command: []string{"--x", "--pprof"},
			Base: &protocol.OverrideBase{Image: "geth:1", Command: []string{"--x"}, HasCommand: true}},
		{Name: "besu", Image: "besu:1", Kind: "el", Team: "besu", Command: []string{"--a"}},
		{Name: "dora", Image: "dora:1"},
		{Name: "debug", Image: "busybox:1", Kind: "tool"},
	}}
	files := &Overrides{Known: true, Deployed: map[string]Deployed{
		"besu":    {Image: "besu:2", Command: []string{"--b"}, HasCommand: true},
		"unknown": {Image: "x:1"},
	}}
	view := Classify(p, facts, files, nil, testNow)

	keep, drop := OverrideKeys(&view, files)
	assert.Equal(t, map[string]Deployed{
		"geth": {Image: "geth:1", Command: []string{"--x"}, HasCommand: true},
		"besu": {Image: "besu:2", Command: []string{"--b"}, HasCommand: true},
	}, keep)
	assert.Equal(t, []string{"debug", "dora"}, drop)
}

// A deploy hands a container to the agent only when the agent rolls it to its target.
func TestDeployState(t *testing.T) {
	p := &Policy{Verbs: []protocol.Verb{protocol.VerbRoll}, DefaultPolicy: DefaultRoll}
	facts := &Facts{Fresh: true, Containers: []protocol.Container{
		{Name: "geth", Image: "geth:1", Digest: "sha256:a", Kind: "el", Team: "geth", Command: []string{"--x"}},
		{Name: "teku", Image: "teku:1", Digest: "sha256:a", Kind: "cl"},
		{Name: "besu", Image: "besu:1", Digest: "sha256:old", Kind: "el"},
		{Name: "nginx", Image: "nginx:1", Kind: "proxy", Linked: true},
		// Unlabelled, rolled or not: Ansible recreates it with its labels.
		{Name: "cadvisor", Image: "cadvisor:1"},
		{Name: "dora", Image: "dora:1", Kind: protocol.KindNone},
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
