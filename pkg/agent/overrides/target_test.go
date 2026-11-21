package overrides

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// configured is what agent.yaml says a container runs.
type configured struct {
	Image   string
	Command []string
}

func TestTargetOf(t *testing.T) {
	m := &configured{Image: "ethpandaops/geth:master"}
	base := []string{"--http", "--maxpeers=50"}
	running := func(image string, command []string, ob *protocol.OverrideBase) *protocol.Container {
		return &protocol.Container{Name: "geth", Image: image, Command: command, Base: ob}
	}
	label := func(t Target, name string) string {
		if v := t.Labels[name]; v != nil {
			return *v
		}

		return "<removed>"
	}

	// Nothing to do.
	got := TargetOf(m.Image, m.Command, running(m.Image, base, nil), nil)
	assert.True(t, got.InSync)
	assert.Nil(t, got.Report(running(m.Image, base, nil), nil))

	// An image and flags, not applied yet.
	entry := &Entry{Image: "ethpandaops/geth:fix", Add: []string{"--pprof"}, Remove: []string{"--maxpeers"}, By: "sam"}
	got = TargetOf(m.Image, m.Command, running(m.Image, base, nil), entry)
	assert.False(t, got.InSync)
	assert.True(t, got.Flags)
	assert.Equal(t, "ethpandaops/geth:fix", got.Image)
	assert.Equal(t, []string{"--http", "--pprof"}, got.Command)
	assert.Equal(t, m.Image, label(got, LabelImage))
	assert.JSONEq(t, `["--http","--maxpeers=50"]`, label(got, LabelCommand))
	assert.False(t, got.Report(running(m.Image, base, nil), entry).Applied)

	// Applied: the labels say what the container was.
	ob := &protocol.OverrideBase{Image: m.Image, Command: base, HasCommand: true}
	applied := running("ethpandaops/geth:fix", []string{"--http", "--pprof"}, ob)
	got = TargetOf(m.Image, m.Command, applied, entry)
	assert.True(t, got.InSync)
	assert.False(t, got.Spec)
	assert.True(t, got.Report(applied, entry).Applied)

	// The entry is gone: back to the configuration, command included.
	got = TargetOf(m.Image, m.Command, applied, nil)
	assert.True(t, got.Restore)
	assert.Equal(t, m.Image, got.Image)
	assert.Equal(t, base, got.Command)
	assert.Equal(t, "<removed>", label(got, LabelImage))
	assert.Equal(t, "<removed>", label(got, LabelCommand))
	assert.Equal(t, &protocol.ContainerOverride{Restoring: true, Image: "ethpandaops/geth:fix"},
		got.Report(applied, nil))

	// Another image without a label is not what agent.yaml says: it is rolled
	// onto the configured one, overrides applied.
	got = TargetOf(m.Image, m.Command, running("me/geth:mine", base, nil), nil)
	assert.True(t, got.Spec)
	assert.False(t, got.Restore)
	assert.Equal(t, m.Image, got.Image)

	got = TargetOf(m.Image, m.Command, running("me/geth:mine", base, nil), &Entry{Add: []string{"--x"}})
	assert.True(t, got.Spec)
	assert.Empty(t, got.Err)
	assert.Equal(t, []string{"--http", "--maxpeers=50", "--x"}, got.Command)

	// Ansible created the container with the override applied, no labels.
	got = TargetOf(m.Image, m.Command, running("ethpandaops/geth:fix", []string{"--http", "--pprof"}, nil), entry)
	assert.True(t, got.InSync, "applying the flags to what has them changes nothing")
	assert.False(t, got.Spec)

	// An entry that cannot be applied changes nothing.
	bad := &Entry{Image: "ethpandaops/geth@sha256:" + strings.Repeat("a", 64)}
	got = TargetOf(m.Image, m.Command, running(m.Image, base, nil), bad)
	assert.True(t, got.InSync)
	assert.Contains(t, got.Err, "pinned to a digest")

	// The need's key follows the image, the digest and the command.
	a := TargetOf(m.Image, m.Command, running(m.Image, base, nil), entry)
	b := TargetOf(m.Image, m.Command, running(m.Image, base, nil), &Entry{Image: "ethpandaops/geth:fix"})
	assert.NotEqual(t, a.Key("sha256:1"), b.Key("sha256:1"))
	assert.NotEqual(t, a.Key("sha256:1"), a.Key("sha256:2"))
	c := TargetOf(m.Image, m.Command, running(m.Image, base, nil), entry)
	assert.Equal(t, a.Key("sha256:1"), c.Key("sha256:1"))
}

// What agent.yaml says (image, command line) is the base the overrides apply
// to; a container that was created otherwise moves there with a spec need.
func TestTargetOfSpec(t *testing.T) {
	m := &configured{Image: "ethpandaops/geth:v2", Command: []string{"--http", "--syncmode=full"}}
	running := func(image string, command ...string) *protocol.Container {
		return &protocol.Container{Name: "geth", Image: image, Command: command}
	}

	got := TargetOf(m.Image, m.Command, running("ethpandaops/geth:v2", "--http", "--syncmode=full"), nil)
	assert.True(t, got.InSync)
	assert.False(t, got.Spec)

	// Ansible rendered a new image and a new flag.
	got = TargetOf(m.Image, m.Command, running("ethpandaops/geth:v1", "--http"), nil)
	assert.True(t, got.Spec)
	assert.True(t, got.Flags)
	assert.False(t, got.Restore)
	assert.Equal(t, "ethpandaops/geth:v2", got.Image)
	assert.Equal(t, []string{"--http", "--syncmode=full"}, got.Command)

	// An override stays: the image override keeps its image, the base moves
	// underneath it.
	entry := &Entry{Image: "ethpandaops/geth:fix", Add: []string{"--pprof"}}
	onOverride := running("ethpandaops/geth:fix", "--http", "--pprof")
	onOverride.Base = &protocol.OverrideBase{Image: "ethpandaops/geth:v1", Command: []string{"--http"}, HasCommand: true}

	got = TargetOf(m.Image, m.Command, onOverride, entry)
	assert.True(t, got.Spec)
	assert.Equal(t, "ethpandaops/geth:fix", got.Image)
	assert.Equal(t, []string{"--http", "--syncmode=full", "--pprof"}, got.Command)

	// Without a command line in agent.yaml the container's own is the base.
	plain := &configured{Image: "ethpandaops/geth:v2"}
	got = TargetOf(plain.Image, plain.Command, running("ethpandaops/geth:v1", "--nat=extip:10.0.0.1"), nil)
	assert.True(t, got.Spec)
	assert.False(t, got.Flags)
}

// Hosts whose command lines differ in per-host values share the key of a
// spec need when the change is the same.
func TestSpecKeyIsShared(t *testing.T) {
	key := func(own string) string {
		c := &protocol.Container{Name: "geth", Image: "ethpandaops/geth:v1", Command: []string{"--http", own}}
		m := &configured{Image: "ethpandaops/geth:v2", Command: []string{"--http", own, "--new"}}
		target := TargetOf(m.Image, m.Command, c, nil)

		return target.SpecKey(c, "sha256:a")
	}

	assert.Equal(t, key("--nat=extip:10.0.0.1"), key("--nat=extip:10.0.0.2"))

	c := &protocol.Container{Name: "geth", Image: "ethpandaops/geth:v1", Command: []string{"--http"}}
	target := TargetOf("ethpandaops/geth:v2", nil, c, nil)
	assert.NotEqual(t, target.SpecKey(c, "sha256:a"), target.SpecKey(c, "sha256:b"), "another digest")

	added, removed := argDelta([]string{"--a", "--b", "--b"}, []string{"--b", "--c", "--c"})
	assert.Equal(t, []string{"--c"}, added)
	assert.Equal(t, []string{"--a"}, removed)
}
