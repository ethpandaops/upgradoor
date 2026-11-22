package plan

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// tgt is targetOf for a container with a team, which may be overridden.
func tgt(d *Deployed, c *protocol.Container, entry *Entry) Target {
	owned := *c
	owned.Kind, owned.Team = protocol.KindEL, "geth"

	return targetOf(d, &owned, entry)
}

// A container without a team runs what was deployed; its entry is reported
// as refused.
func TestTargetOfWithoutTeam(t *testing.T) {
	d := &Deployed{Image: "ethpandaops/tool:v2", Command: []string{"--a"}, HasCommand: true}
	entry := &Entry{Image: "me/tool:debug", By: "sam"}

	c := &protocol.Container{Name: "tool", Image: "ethpandaops/tool:v2", Command: []string{"--a"}, Kind: "tool"}
	got := targetOf(d, c, entry)
	assert.True(t, got.InSync)
	assert.Contains(t, got.Refused, "no "+protocol.LabelTeam+" label")
	assert.Empty(t, got.Err)

	report := got.report(c, entry)
	assert.False(t, report.Applied)
	assert.Contains(t, report.Error, "no "+protocol.LabelTeam+" label")

	// A container that still runs an override goes back to what was deployed.
	onOverride := &protocol.Container{
		Name: "tool", Image: "me/tool:debug", Command: []string{"--a"}, Kind: "tool",
		Base: &protocol.OverrideBase{Image: "ethpandaops/tool:v2"},
	}
	got = targetOf(d, onOverride, entry)
	assert.True(t, got.Restore)
	assert.Equal(t, "ethpandaops/tool:v2", got.Image)
	assert.Nil(t, got.Base, "the override labels are removed")
}

// Without a deployed entry a container counts as deployed with what it was created with.
func TestTargetOfWithoutDeployed(t *testing.T) {
	c := &protocol.Container{Name: "geth", Image: "ethpandaops/geth:master", Command: []string{"--http"}}
	got := tgt(nil, c, nil)
	assert.True(t, got.InSync)
	assert.False(t, got.Spec)
}

func TestTargetOf(t *testing.T) {
	m := &Deployed{Image: "ethpandaops/geth:master"}
	base := []string{"--http", "--maxpeers=50"}
	running := func(image string, command []string, ob *protocol.OverrideBase) *protocol.Container {
		return &protocol.Container{Name: "geth", Image: image, Command: command, Base: ob}
	}

	// Nothing to do.
	got := tgt(m, running(m.Image, base, nil), nil)
	assert.True(t, got.InSync)
	assert.Nil(t, got.report(running(m.Image, base, nil), nil))

	// An image and a command, not applied yet.
	entry := &Entry{Image: "ethpandaops/geth:fix", Command: []string{"--http", "--pprof"}, HasCommand: true, By: "sam"}
	got = tgt(m, running(m.Image, base, nil), entry)
	assert.False(t, got.InSync)
	assert.True(t, got.Flags)
	assert.Equal(t, "ethpandaops/geth:fix", got.Image)
	assert.Equal(t, []string{"--http", "--pprof"}, got.Command)
	assert.Equal(t, &protocol.OverrideBase{Image: m.Image, Command: base, HasCommand: true}, got.Base,
		"the labels record what the container was")

	report := got.report(running(m.Image, base, nil), entry)
	assert.False(t, report.Applied)
	assert.Equal(t, []string{"--pprof"}, report.AddFlags, "reported against the default command")
	assert.Equal(t, []string{"--maxpeers=50"}, report.RemoveFlags)

	// Applied: the labels say what the container was.
	ob := &protocol.OverrideBase{Image: m.Image, Command: base, HasCommand: true}
	applied := running("ethpandaops/geth:fix", []string{"--http", "--pprof"}, ob)
	got = tgt(m, applied, entry)
	assert.True(t, got.InSync)
	assert.False(t, got.Spec)
	assert.True(t, got.report(applied, entry).Applied)

	// The entry is gone: back to the configuration, command included.
	got = tgt(m, applied, nil)
	assert.True(t, got.Restore)
	assert.Equal(t, m.Image, got.Image)
	assert.Equal(t, base, got.Command)
	assert.Nil(t, got.Base, "both labels are removed")
	assert.Equal(t, &protocol.ContainerOverride{Restoring: true, Image: "ethpandaops/geth:fix"},
		got.report(applied, nil))

	// Only an image: only the image label.
	got = tgt(m, running(m.Image, base, nil), &Entry{Image: "ethpandaops/geth:fix"})
	assert.Equal(t, &protocol.OverrideBase{Image: m.Image}, got.Base)

	// Another image without a label is rolled onto the configured one.
	got = tgt(m, running("me/geth:mine", base, nil), nil)
	assert.True(t, got.Spec)
	assert.False(t, got.Restore)
	assert.Equal(t, m.Image, got.Image)

	got = tgt(m, running("me/geth:mine", base, nil), &Entry{Command: []string{"--x"}, HasCommand: true})
	assert.True(t, got.Spec)
	assert.Empty(t, got.Err)
	assert.Equal(t, []string{"--x"}, got.Command, "the command replaces the whole line")

	// Ansible created the container with the override applied, no labels.
	got = tgt(m, running("ethpandaops/geth:fix", []string{"--http", "--pprof"}, nil), entry)
	assert.True(t, got.InSync, "a container that runs the override's command is in sync")
	assert.False(t, got.Spec)

	// An entry that cannot be applied changes nothing.
	bad := &Entry{Image: "ethpandaops/geth@sha256:" + strings.Repeat("a", 64)}
	got = tgt(m, running(m.Image, base, nil), bad)
	assert.True(t, got.InSync)
	assert.Contains(t, got.Err, "pinned to a digest")

	// The need's key follows the image, the digest and the command.
	a := tgt(m, running(m.Image, base, nil), entry)
	b := tgt(m, running(m.Image, base, nil), &Entry{Image: "ethpandaops/geth:fix"})
	assert.NotEqual(t, a.key("sha256:1"), b.key("sha256:1"))
	assert.NotEqual(t, a.key("sha256:1"), a.key("sha256:2"))
	c := tgt(m, running(m.Image, base, nil), entry)
	assert.Equal(t, a.key("sha256:1"), c.key("sha256:1"))
}

// Overrides apply to the deployed form; a container created otherwise gets a spec target.
func TestTargetOfSpec(t *testing.T) {
	m := &Deployed{Image: "ethpandaops/geth:v2", Command: []string{"--http", "--syncmode=full"}, HasCommand: true}
	running := func(image string, command ...string) *protocol.Container {
		return &protocol.Container{Name: "geth", Image: image, Command: command}
	}

	got := tgt(m, running("ethpandaops/geth:v2", "--http", "--syncmode=full"), nil)
	assert.True(t, got.InSync)
	assert.False(t, got.Spec)

	// Ansible rendered a new image and a new flag.
	got = tgt(m, running("ethpandaops/geth:v1", "--http"), nil)
	assert.True(t, got.Spec)
	assert.True(t, got.Flags)
	assert.False(t, got.Restore)
	assert.Equal(t, "ethpandaops/geth:v2", got.Image)
	assert.Equal(t, []string{"--http", "--syncmode=full"}, got.Command)
	assert.Nil(t, got.Base, "it runs what was deployed: no override labels")

	// An override's command stays as written while the base moves underneath it.
	entry := &Entry{Image: "ethpandaops/geth:fix", Command: []string{"--http", "--pprof"}, HasCommand: true}
	onOverride := running("ethpandaops/geth:v1", "--http")
	onOverride.Base = &protocol.OverrideBase{Image: "ethpandaops/geth:v1", Command: []string{"--http"}, HasCommand: true}

	got = tgt(m, onOverride, entry)
	assert.True(t, got.Spec)
	assert.Equal(t, "ethpandaops/geth:fix", got.Image)
	assert.Equal(t, []string{"--http", "--pprof"}, got.Command)
	assert.Equal(t, &protocol.OverrideBase{
		Image: "ethpandaops/geth:v2", Command: []string{"--http", "--syncmode=full"}, HasCommand: true,
	}, got.Base, "the labels record the new deployed form")

	// After that recreate the container is in sync.
	recreated := running("ethpandaops/geth:fix", "--http", "--pprof")
	recreated.Base = got.Base
	got = tgt(m, recreated, entry)
	assert.True(t, got.InSync)
	assert.False(t, got.Spec)
	assert.False(t, got.Restore)

	// Only an image: the deployed command.
	got = tgt(m, running("ethpandaops/geth:v2", "--http", "--syncmode=full"), &Entry{Image: "ethpandaops/geth:fix"})
	assert.Equal(t, []string{"--http", "--syncmode=full"}, got.Command)

	// Without a command line in agent.yaml the container's own is the base.
	plain := &Deployed{Image: "ethpandaops/geth:v2"}
	got = tgt(plain, running("ethpandaops/geth:v1", "--nat=extip:10.0.0.1"), nil)
	assert.True(t, got.Spec)
	assert.False(t, got.Flags)
}

// Hosts making the same change share the spec key, whatever their per-host values.
func TestSpecKeyIsShared(t *testing.T) {
	key := func(own string) string {
		c := &protocol.Container{Name: "geth", Image: "ethpandaops/geth:v1", Command: []string{"--http", own}}
		m := &Deployed{Image: "ethpandaops/geth:v2", Command: []string{"--http", own, "--new"}, HasCommand: true}
		target := tgt(m, c, nil)

		return target.specKey(c, "sha256:a")
	}

	assert.Equal(t, key("--nat=extip:10.0.0.1"), key("--nat=extip:10.0.0.2"))

	c := &protocol.Container{Name: "geth", Image: "ethpandaops/geth:v1", Command: []string{"--http"}}
	target := tgt(&Deployed{Image: "ethpandaops/geth:v2"}, c, nil)
	assert.NotEqual(t, target.specKey(c, "sha256:a"), target.specKey(c, "sha256:b"), "another digest")

	added, removed := argDelta([]string{"--a", "--b", "--b"}, []string{"--b", "--c", "--c"})
	assert.Equal(t, []string{"--c"}, added)
	assert.Equal(t, []string{"--a"}, removed)
}
