package overrides

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The agent changes the entry it is asked to change and no other byte.
func TestTextSet(t *testing.T) {
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

	text := NewText([]byte(hand))
	entry := &Entry{
		Image: "ethpandaops/geth:fix", Add: []string{"--a=b", "--c \"d\""}, By: "sam@example.org",
		Reason: "why: because", Until: untilOf(time.Hour),
	}
	require.NoError(t, text.Set("geth", entry, overrideTime, "sam@example.org",
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
	assert.Equal(t, want, string(text.Bytes()))

	set, err := Parse(text.Bytes())
	require.NoError(t, err)
	assert.Equal(t, entry.Add, set.Entry("geth").Add)

	// Removing it leaves the history, and the other entry as it was.
	require.NoError(t, text.Set("geth", nil, overrideTime.Add(time.Hour), "upgradoor", "expired"))

	set, err = Parse(text.Bytes())
	require.NoError(t, err)
	assert.Nil(t, set.Entry("geth"))
	assert.Equal(t, "sigp/lighthouse:unstable", set.Entry("beacon").Image)
	assert.Contains(t, string(text.Bytes()),
		"  # [geth] 2026-11-20T16:00:00Z sam@example.org: image ethpandaops/geth:fix (was ethpandaops/geth:old)\n"+
			"  # [geth] 2026-11-20T17:00:00Z upgradoor: expired\n\n# the end\n")
	assert.True(t, strings.HasPrefix(string(text.Bytes()), block), "the generated block is kept byte for byte")

	// Only the newest history lines are kept.
	for i := range 2 * maxHistory {
		require.NoError(t, text.Set("geth", entry, overrideTime.Add(time.Duration(i)*time.Minute), "sam", "again"))
	}

	assert.Equal(t, maxHistory, strings.Count(string(text.Bytes()), "# [geth] "))
	assert.NotContains(t, string(text.Bytes()), "expired")
	assert.Contains(t, string(text.Bytes()), "# keep an eye on this one\n  beacon:\n")
}

func TestTextSetWhereThereIsNothing(t *testing.T) {
	entry := &Entry{Image: "a/b:c", By: "x", Reason: "y"}

	for name, tt := range map[string]struct{ before, want string }{
		"an empty file": {"", "overrides:\n  # [geth] 2026-11-20T16:00:00Z x: n\n  \"geth\":\n" +
			"    image: \"a/b:c\"\n    by: \"x\"\n    reason: \"y\"\n"},
		"an empty mapping": {"overrides: {}\n", "overrides:\n  # [geth] 2026-11-20T16:00:00Z x: n\n" +
			"  \"geth\":\n    image: \"a/b:c\"\n    by: \"x\"\n    reason: \"y\"\n"},
		"comments only": {"# notes", "# notes\n\noverrides:\n  # [geth] 2026-11-20T16:00:00Z x: n\n" +
			"  \"geth\":\n    image: \"a/b:c\"\n    by: \"x\"\n    reason: \"y\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			text := NewText([]byte(tt.before))
			require.NoError(t, text.Set("geth", entry, overrideTime, "x", "n"))
			assert.Equal(t, tt.want, string(text.Bytes()))
		})
	}

	text := NewText([]byte("overrides: {geth: {image: a}}\n"))
	require.ErrorContains(t, text.Set("geth", entry, overrideTime, "x", "n"), "indented block")

	text = NewText([]byte("overrides: [broken\n"))
	require.Error(t, text.Set("geth", entry, overrideTime, "x", "n"))
}

func TestGeneratedBlockIsReplaced(t *testing.T) {
	old := strings.Join(GeneratedBlock([]GeneratedContainer{{Name: "geth", Image: "a/geth:1"}}), "\n")
	rest := "\noverrides:\n  geth:\n    image: a/geth:2\n"

	text := NewText([]byte(old + "\n" + rest))
	block := GeneratedBlock([]GeneratedContainer{{Name: "geth", Image: "a/geth:3", Command: []string{"--x"}}})
	text.SetGeneratedBlock(block)
	assert.Equal(t, strings.Join(block, "\n")+"\n"+rest, string(text.Bytes()))

	text = NewText([]byte("overrides:\n"))
	text.SetGeneratedBlock(block)
	assert.Equal(t, strings.Join(block, "\n")+"\n\noverrides:\n", string(text.Bytes()))

	set, err := Parse([]byte(strings.Join(block, "\n")))
	require.NoError(t, err)
	assert.Empty(t, set.Entries, "the block is comments only")
}

func TestFileKeepsItsMode(t *testing.T) {
	file := filepath.Join(t.TempDir(), "overrides.yaml")
	f := NewFile(file)
	entry := &Entry{Image: "a/b:c", By: "x", Reason: "y"}

	require.NoError(t, f.Change(func(text *Text) error {
		return text.Set("geth", entry, overrideTime, "x", "set")
	}))

	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o664), info.Mode().Perm(), "a new file is for the entry user to edit")

	require.NoError(t, os.Chmod(file, 0o640))
	require.NoError(t, f.Change(func(text *Text) error {
		return text.Set("geth", nil, overrideTime, "x", "cleared")
	}))

	info, err = os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	assert.Empty(t, Read(file).Entries)
}
