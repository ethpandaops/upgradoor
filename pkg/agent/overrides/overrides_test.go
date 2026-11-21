package overrides

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

func TestParse(t *testing.T) {
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
			set, err := Parse([]byte(content))
			require.NoError(t, err)
			assert.True(t, set.Known)
			assert.True(t, set.Entry("geth").Empty())
		})
	}

	set, err := Parse([]byte(block + `
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

	// The deployed block is read back as written.
	assert.Equal(t, &Deployed{
		Image: "ethpandaops/geth:master", Command: []string{"--http", "--a=b c"}, HasCommand: true,
	}, set.DeployedOf("geth"))
	assert.Equal(t, &Deployed{Image: "sigp/lighthouse:latest", Command: []string{}, HasCommand: true},
		set.DeployedOf("beacon"))
	assert.Nil(t, set.DeployedOf("nginx"))

	// What Ansible writes: an entry without a command keeps the container's.
	own, err := Parse([]byte("deployed:\n  geth:\n    image: \"a/geth:1\"\n"))
	require.NoError(t, err)
	assert.Equal(t, &Deployed{Image: "a/geth:1"}, own.DeployedOf("geth"))

	_, err = Parse([]byte("deployed:\n  geth:\n    command: []\n"))
	require.ErrorContains(t, err, "names no image")

	geth := set.Entry("geth")
	require.NotNil(t, geth)
	assert.Equal(t, Entry{
		Image: "ethpandaops/geth:fix", Add: []string{"--verbosity=4", "--pprof"},
		Remove: []string{"--maxpeers"}, By: "sam", Reason: "sync: stuck", Until: untilOf(24 * time.Hour),
	}, *geth)
	assert.Nil(t, set.Entry("beacon").Until, "an end is optional by hand")
	assert.False(t, geth.Expired(overrideTime))
	assert.True(t, geth.Expired(overrideTime.Add(24*time.Hour)))

	_, err = Parse([]byte("overrides:\n  geth:\n    flag: [a]\n"))
	require.ErrorContains(t, err, "field flag not found")

	_, err = Parse([]byte("overrides: [geth\n"))
	require.Error(t, err)

	// A key that is not known is an error, not "no overrides": a typo must
	// not put every container back.
	_, err = Parse([]byte("overides:\n  geth:\n    image: a/b:c\n"))
	require.ErrorContains(t, err, "field overides not found")

	missing := Read(filepath.Join(t.TempDir(), "none.yaml"))
	assert.True(t, missing.Known, "no file is no overrides")

	broken := filepath.Join(t.TempDir(), "o.yaml")
	require.NoError(t, os.WriteFile(broken, []byte("overrides: [geth\n"), 0o600))
	assert.False(t, Read(broken).Known, "a broken file is not no overrides")
}

func TestBaseFromLabels(t *testing.T) {
	assert.Nil(t, BaseFromLabels(nil))
	assert.Nil(t, BaseFromLabels(map[string]string{LabelCommand: "not a list"}), "no record")
	assert.Equal(t, &protocol.OverrideBase{Image: "a/b:c"}, BaseFromLabels(map[string]string{LabelImage: "a/b:c"}))
	assert.Equal(t, &protocol.OverrideBase{Command: []string{"--x"}, HasCommand: true},
		BaseFromLabels(map[string]string{LabelCommand: `["--x"]`}))
}
