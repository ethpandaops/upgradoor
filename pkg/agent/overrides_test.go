package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

var overrideTime = time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)

func untilOf(d time.Duration) *time.Time {
	t := overrideTime.Add(d)

	return &t
}

func TestParseOverrides(t *testing.T) {
	block := strings.Join(GeneratedBlock([]GeneratedContainer{
		{Name: "geth", Image: "ethpandaops/geth:master", Command: []string{"--http", "--a=b c"}},
		{Name: "beacon", Image: "sigp/lighthouse:latest"},
	}), "\n") + "\n"

	for name, content := range map[string]string{
		"empty":                "",
		"the generated block":  block,
		"the key alone":        block + "\noverrides:\n",
		"an empty mapping":     "overrides: {}\n",
		"only comments in it":  "overrides:\n  # [geth] 2026-11-20T16:00:00Z sam: cleared\n",
		"an entry without any": "overrides:\n  geth:\n",
	} {
		t.Run(name, func(t *testing.T) {
			set, err := parseOverrides([]byte(content))
			require.NoError(t, err)
			assert.True(t, set.known)
			assert.True(t, set.entry("geth").empty())
		})
	}

	set, err := parseOverrides([]byte(block + `
overrides:
  geth:
    image: ethpandaops/geth:fix
    flags:
      add: ["--verbosity=4", --pprof]
      remove:
        - --maxpeers
    by: sam
    reason: "sync: stuck"
    until: 2026-11-21T16:00:00Z
  beacon:
    image: sigp/lighthouse:unstable
`))
	require.NoError(t, err)

	geth := set.entry("geth")
	require.NotNil(t, geth)
	assert.Equal(t, Override{
		Image: "ethpandaops/geth:fix", Add: []string{"--verbosity=4", "--pprof"},
		Remove: []string{"--maxpeers"}, By: "sam", Reason: "sync: stuck", Until: untilOf(24 * time.Hour),
	}, *geth)
	assert.Nil(t, set.entry("beacon").Until, "an end is optional by hand")
	assert.False(t, geth.expired(overrideTime))
	assert.True(t, geth.expired(overrideTime.Add(24*time.Hour)))

	_, err = parseOverrides([]byte("overrides:\n  geth:\n    flag: [a]\n"))
	require.ErrorContains(t, err, "field flag not found")

	_, err = parseOverrides([]byte("overrides: [geth\n"))
	require.Error(t, err)

	// A key that is not known is an error, not "no overrides": a typo must
	// not put every container back.
	_, err = parseOverrides([]byte("overides:\n  geth:\n    image: a/b:c\n"))
	require.ErrorContains(t, err, "field overides not found")

	missing := readOverrides(filepath.Join(t.TempDir(), "none.yaml"))
	assert.True(t, missing.known, "no file is no overrides")

	broken := filepath.Join(t.TempDir(), "o.yaml")
	require.NoError(t, os.WriteFile(broken, []byte("overrides: [geth\n"), 0o600))
	assert.False(t, readOverrides(broken).known, "a broken file is not no overrides")
}

// The agent changes the entry it is asked to change and no other byte.
func TestOverridesTextSet(t *testing.T) {
	block := strings.Join(GeneratedBlock(nil), "\n") + "\n"
	hand := block + `
# my notes
overrides:

  # keep an eye on this one
  beacon:
      image: sigp/lighthouse:unstable   # indented my way
      reason: testing

      by: me
  geth:
    image: ethpandaops/geth:old
    by: me

# the end
`

	text := newOverridesText([]byte(hand))
	entry := &Override{
		Image: "ethpandaops/geth:fix", Add: []string{"--a=b", "--c \"d\""}, By: "sam@example.org",
		Reason: "why: because", Until: untilOf(time.Hour),
	}
	require.NoError(t, text.set("geth", entry, overrideTime, "sam@example.org",
		"image ethpandaops/geth:fix (was ethpandaops/geth:old)"))

	want := block + `
# my notes
overrides:

  # keep an eye on this one
  beacon:
      image: sigp/lighthouse:unstable   # indented my way
      reason: testing

      by: me
  # [geth] 2026-11-20T16:00:00Z sam@example.org: image ethpandaops/geth:fix (was ethpandaops/geth:old)
  "geth":
    image: "ethpandaops/geth:fix"
    flags:
      add: ["--a=b", "--c \"d\""]
    by: "sam@example.org"
    reason: "why: because"
    until: 2026-11-20T17:00:00Z

# the end
`
	assert.Equal(t, want, string(text.bytes()))

	set, err := parseOverrides(text.bytes())
	require.NoError(t, err)
	assert.Equal(t, entry.Add, set.entry("geth").Add)

	// Removing it leaves the history, and the other entry as it was.
	require.NoError(t, text.set("geth", nil, overrideTime.Add(time.Hour), "upgradoor", "expired"))

	set, err = parseOverrides(text.bytes())
	require.NoError(t, err)
	assert.Nil(t, set.entry("geth"))
	assert.Equal(t, "sigp/lighthouse:unstable", set.entry("beacon").Image)
	assert.Contains(t, string(text.bytes()),
		"  # [geth] 2026-11-20T16:00:00Z sam@example.org: image ethpandaops/geth:fix (was ethpandaops/geth:old)\n"+
			"  # [geth] 2026-11-20T17:00:00Z upgradoor: expired\n\n# the end\n")
	assert.True(t, strings.HasPrefix(string(text.bytes()), block), "the generated block is kept byte for byte")

	// Only the newest history lines are kept.
	for i := range 2 * maxOverrideHistory {
		require.NoError(t, text.set("geth", entry, overrideTime.Add(time.Duration(i)*time.Minute), "sam", "again"))
	}

	assert.Equal(t, maxOverrideHistory, strings.Count(string(text.bytes()), "# [geth] "))
	assert.NotContains(t, string(text.bytes()), "expired")
	assert.Contains(t, string(text.bytes()), "# keep an eye on this one\n  beacon:\n")
}

func TestOverridesTextSetWhereThereIsNothing(t *testing.T) {
	entry := &Override{Image: "a/b:c", By: "x", Reason: "y"}

	for name, tt := range map[string]struct{ before, want string }{
		"an empty file": {"", "overrides:\n  # [geth] 2026-11-20T16:00:00Z x: n\n  \"geth\":\n" +
			"    image: \"a/b:c\"\n    by: \"x\"\n    reason: \"y\"\n"},
		"an empty mapping": {"overrides: {}\n", "overrides:\n  # [geth] 2026-11-20T16:00:00Z x: n\n" +
			"  \"geth\":\n    image: \"a/b:c\"\n    by: \"x\"\n    reason: \"y\"\n"},
		"comments only": {"# notes", "# notes\n\noverrides:\n  # [geth] 2026-11-20T16:00:00Z x: n\n" +
			"  \"geth\":\n    image: \"a/b:c\"\n    by: \"x\"\n    reason: \"y\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			text := newOverridesText([]byte(tt.before))
			require.NoError(t, text.set("geth", entry, overrideTime, "x", "n"))
			assert.Equal(t, tt.want, string(text.bytes()))
		})
	}

	text := newOverridesText([]byte("overrides: {geth: {image: a}}\n"))
	require.ErrorContains(t, text.set("geth", entry, overrideTime, "x", "n"), "indented block")

	text = newOverridesText([]byte("overrides: [broken\n"))
	require.Error(t, text.set("geth", entry, overrideTime, "x", "n"))
}

func TestGeneratedBlockIsReplaced(t *testing.T) {
	old := strings.Join(GeneratedBlock([]GeneratedContainer{{Name: "geth", Image: "a/geth:1"}}), "\n")
	rest := "\noverrides:\n  geth:\n    image: a/geth:2\n"

	text := newOverridesText([]byte(old + "\n" + rest))
	block := GeneratedBlock([]GeneratedContainer{{Name: "geth", Image: "a/geth:3", Command: []string{"--x"}}})
	text.setGeneratedBlock(block)
	assert.Equal(t, strings.Join(block, "\n")+"\n"+rest, string(text.bytes()))

	text = newOverridesText([]byte("overrides:\n"))
	text.setGeneratedBlock(block)
	assert.Equal(t, strings.Join(block, "\n")+"\n\noverrides:\n", string(text.bytes()))

	set, err := parseOverrides([]byte(strings.Join(block, "\n")))
	require.NoError(t, err)
	assert.Empty(t, set.entries, "the block is comments only")
}

func TestImageAllowed(t *testing.T) {
	patterns := []string{"ethpandaops/geth", "ghcr.io/ethereum/*", "registry.example:5000/team/*-debug"}

	for ref, want := range map[string]bool{
		"ethpandaops/geth:fix":                    true,
		"docker.io/ethpandaops/geth:fix":          true,
		"index.docker.io/ethpandaops/geth":        true,
		"ethpandaops/geth-debug:fix":              false,
		"ethpandaops/besu:fix":                    false,
		"ghcr.io/ethereum/go-ethereum:x":          true,
		"ghcr.io/ethereum/a/b:x":                  false,
		"ghcr.io/other/go-ethereum:x":             false,
		"registry.example:5000/team/geth-debug:1": true,
		"registry.example:5000/team/geth:1":       false,
		"evil.example/ethpandaops/geth:fix":       false,
		"not a reference":                         false,
	} {
		assert.Equal(t, want, imageAllowed(patterns, ref), ref)
	}

	assert.False(t, imageAllowed(nil, "ethpandaops/geth:fix"), "no pattern allows nothing")
	assert.True(t, imageAllowed([]string{"nginx"}, "nginx:1"), "an official image")

	require.NoError(t, validateRepositoryPattern("registry.example:5000/team/*"))
	require.Error(t, validateRepositoryPattern("ethpandaops/geth:master"))
	require.Error(t, validateRepositoryPattern("ethpandaops/[geth"))
	require.Error(t, validateRepositoryPattern(""))
}

func TestTargetOf(t *testing.T) {
	m := &ManagedContainer{Name: "geth", Image: "ethpandaops/geth:master"}
	base := []string{"--http", "--maxpeers=50"}
	running := func(image string, command []string, ob *protocol.OverrideBase) *protocol.Container {
		return &protocol.Container{Name: "geth", Image: image, Command: command, Base: ob}
	}
	label := func(t overrideTarget, name string) string {
		if v := t.labels[name]; v != nil {
			return *v
		}

		return "<removed>"
	}

	// Nothing to do.
	got := targetOf(m, running(m.Image, base, nil), nil)
	assert.True(t, got.inSync)
	assert.Nil(t, got.report(running(m.Image, base, nil), nil))

	// An image and flags, not applied yet.
	entry := &Override{Image: "ethpandaops/geth:fix", Add: []string{"--pprof"}, Remove: []string{"--maxpeers"}, By: "sam"}
	got = targetOf(m, running(m.Image, base, nil), entry)
	assert.False(t, got.inSync)
	assert.True(t, got.flags)
	assert.Equal(t, "ethpandaops/geth:fix", got.image)
	assert.Equal(t, []string{"--http", "--pprof"}, got.command)
	assert.Equal(t, m.Image, label(got, LabelOverrideImage))
	assert.JSONEq(t, `["--http","--maxpeers=50"]`, label(got, LabelOverrideCommand))
	assert.False(t, got.report(running(m.Image, base, nil), entry).Applied)

	// Applied: the labels say what the container was.
	ob := &protocol.OverrideBase{Image: m.Image, Command: base, HasCommand: true}
	applied := running("ethpandaops/geth:fix", []string{"--http", "--pprof"}, ob)
	got = targetOf(m, applied, entry)
	assert.True(t, got.inSync)
	assert.False(t, got.spec)
	assert.True(t, got.report(applied, entry).Applied)

	// The entry is gone: back to the configuration, command included.
	got = targetOf(m, applied, nil)
	assert.True(t, got.restore)
	assert.Equal(t, m.Image, got.image)
	assert.Equal(t, base, got.command)
	assert.Equal(t, "<removed>", label(got, LabelOverrideImage))
	assert.Equal(t, "<removed>", label(got, LabelOverrideCommand))
	assert.Equal(t, &protocol.ContainerOverride{Restoring: true, Image: "ethpandaops/geth:fix"},
		got.report(applied, nil))

	// Another image without a label is not what agent.yaml says: it is rolled
	// onto the configured one, overrides applied.
	got = targetOf(m, running("me/geth:mine", base, nil), nil)
	assert.True(t, got.spec)
	assert.False(t, got.restore)
	assert.Equal(t, m.Image, got.image)

	got = targetOf(m, running("me/geth:mine", base, nil), &Override{Add: []string{"--x"}})
	assert.True(t, got.spec)
	assert.Empty(t, got.err)
	assert.Equal(t, []string{"--http", "--maxpeers=50", "--x"}, got.command)

	// Ansible created the container with the override applied, no labels.
	got = targetOf(m, running("ethpandaops/geth:fix", []string{"--http", "--pprof"}, nil), entry)
	assert.True(t, got.inSync, "applying the flags to what has them changes nothing")
	assert.False(t, got.spec)

	// An entry that cannot be applied changes nothing.
	bad := &Override{Image: "ethpandaops/geth@sha256:" + strings.Repeat("a", 64)}
	got = targetOf(m, running(m.Image, base, nil), bad)
	assert.True(t, got.inSync)
	assert.Contains(t, got.err, "pinned to a digest")

	// The need's key follows the image, the digest and the command.
	a := targetOf(m, running(m.Image, base, nil), entry)
	b := targetOf(m, running(m.Image, base, nil), &Override{Image: "ethpandaops/geth:fix"})
	assert.NotEqual(t, a.key("sha256:1"), b.key("sha256:1"))
	assert.NotEqual(t, a.key("sha256:1"), a.key("sha256:2"))
	c := targetOf(m, running(m.Image, base, nil), entry)
	assert.Equal(t, a.key("sha256:1"), c.key("sha256:1"))
}

func TestOverridesFileKeepsItsMode(t *testing.T) {
	file := filepath.Join(t.TempDir(), "overrides.yaml")
	f := &overridesFile{path: file}
	entry := &Override{Image: "a/b:c", By: "x", Reason: "y"}

	require.NoError(t, f.change(func(text *overridesText) error {
		return text.set("geth", entry, overrideTime, "x", "set")
	}))

	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o664), info.Mode().Perm(), "a new file is for the entry user to edit")

	require.NoError(t, os.Chmod(file, 0o640))
	require.NoError(t, f.change(func(text *overridesText) error {
		return text.set("geth", nil, overrideTime, "x", "cleared")
	}))

	info, err = os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	assert.Empty(t, readOverrides(file).entries)
}

// What agent.yaml says (image, command line) is the base the overrides apply
// to; a container that was created otherwise moves there with a spec need.
func TestTargetOfSpec(t *testing.T) {
	m := &ManagedContainer{Name: "geth", Image: "ethpandaops/geth:v2", Command: []string{"--http", "--syncmode=full"}}
	running := func(image string, command ...string) *protocol.Container {
		return &protocol.Container{Name: "geth", Image: image, Command: command}
	}

	got := targetOf(m, running("ethpandaops/geth:v2", "--http", "--syncmode=full"), nil)
	assert.True(t, got.inSync)
	assert.False(t, got.spec)

	// Ansible rendered a new image and a new flag.
	got = targetOf(m, running("ethpandaops/geth:v1", "--http"), nil)
	assert.True(t, got.spec)
	assert.True(t, got.flags)
	assert.False(t, got.restore)
	assert.Equal(t, "ethpandaops/geth:v2", got.image)
	assert.Equal(t, []string{"--http", "--syncmode=full"}, got.command)

	// An override stays: the image override keeps its image, the base moves
	// underneath it.
	entry := &Override{Image: "ethpandaops/geth:fix", Add: []string{"--pprof"}}
	onOverride := running("ethpandaops/geth:fix", "--http", "--pprof")
	onOverride.Base = &protocol.OverrideBase{Image: "ethpandaops/geth:v1", Command: []string{"--http"}, HasCommand: true}

	got = targetOf(m, onOverride, entry)
	assert.True(t, got.spec)
	assert.Equal(t, "ethpandaops/geth:fix", got.image)
	assert.Equal(t, []string{"--http", "--syncmode=full", "--pprof"}, got.command)

	// Without a command line in agent.yaml the container's own is the base.
	plain := &ManagedContainer{Name: "geth", Image: "ethpandaops/geth:v2"}
	got = targetOf(plain, running("ethpandaops/geth:v1", "--nat=extip:10.0.0.1"), nil)
	assert.True(t, got.spec)
	assert.False(t, got.flags)
}

// Hosts whose command lines differ in per-host values share the key of a
// spec need when the change is the same.
func TestSpecKeyIsShared(t *testing.T) {
	key := func(own string) string {
		c := &protocol.Container{Name: "geth", Image: "ethpandaops/geth:v1", Command: []string{"--http", own}}
		m := &ManagedContainer{Name: "geth", Image: "ethpandaops/geth:v2", Command: []string{"--http", own, "--new"}}
		target := targetOf(m, c, nil)

		return target.specKey(c, "sha256:a")
	}

	assert.Equal(t, key("--nat=extip:10.0.0.1"), key("--nat=extip:10.0.0.2"))

	c := &protocol.Container{Name: "geth", Image: "ethpandaops/geth:v1", Command: []string{"--http"}}
	target := targetOf(&ManagedContainer{Name: "geth", Image: "ethpandaops/geth:v2"}, c, nil)
	assert.NotEqual(t, target.specKey(c, "sha256:a"), target.specKey(c, "sha256:b"), "another digest")

	added, removed := argDelta([]string{"--a", "--b", "--b"}, []string{"--b", "--c", "--c"})
	assert.Equal(t, []string{"--c"}, added)
	assert.Equal(t, []string{"--a"}, removed)
}
