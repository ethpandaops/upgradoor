package plan

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLine(t *testing.T) {
	assert.Equal(t, "first line", Line("  first line \nsecond"))
	assert.Equal(t, "a b", Line("a\tb"))
	assert.Equal(t, "a b", Line("a b"), "line separator")
	assert.Equal(t, "a b", Line("a b"), "paragraph separator")
	assert.Equal(t, "a b", Line("a\u0085b"), "next line")
	assert.Equal(t, "bad ? byte", Line("bad \xff byte"))
	assert.Len(t, Line(strings.Repeat("x", 5000)), maxLine)
	assert.Len(t, Line("x"+strings.Repeat("ä", 300)), maxLine-1, "a rune cut in two is dropped")
	assert.Equal(t, "refused", ErrRefused.Error())

	err := Refusal("container %s is not managed", "geth")
	require.ErrorIs(t, err, ErrRefused)
	assert.Equal(t, "refused: container geth is not managed", err.Error())
	assert.False(t, errors.Is(errors.New("refused"), ErrRefused))
}

func TestEntryValidate(t *testing.T) {
	require.NoError(t, (&Entry{Image: "ethpandaops/geth:fix"}).Validate())
	require.NoError(t, (&Entry{Add: []string{"--a"}, By: strings.Repeat("x", maxEntryText)}).Validate())

	for name, tt := range map[string]struct {
		entry *Entry
		want  string
	}{
		"nil":            {nil, "names an image, flags or both"},
		"empty":          {&Entry{By: "sam"}, "names an image, flags or both"},
		"no reference":   {&Entry{Image: "UPPER/case:x"}, "invalid"},
		"a digest":       {&Entry{Image: "ethpandaops/geth@sha256:" + strings.Repeat("a", 64)}, "pinned to a digest"},
		"a bad flag":     {&Entry{Remove: []string{"--a b c"}}, "one value"},
		"a long by":      {&Entry{Add: []string{"--a"}, By: strings.Repeat("x", maxEntryText+1)}, "too long"},
		"a long reason:": {&Entry{Add: []string{"--a"}, Reason: strings.Repeat("x", maxEntryText+1)}, "too long"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, tt.entry.Validate(), tt.want)
		})
	}
}

func TestEntryDescribe(t *testing.T) {
	assert.Equal(t, NoOverride, (*Entry)(nil).Describe())
	assert.Equal(t, NoOverride, (&Entry{By: "sam"}).Describe())
	assert.Equal(t, "image ethpandaops/geth:fix", (&Entry{Image: "ethpandaops/geth:fix"}).Describe())
	assert.Equal(t, "image ethpandaops/geth:fix, flags +--verbosity=4 ---maxpeers",
		(&Entry{Image: "ethpandaops/geth:fix", Add: []string{"--verbosity=4"}, Remove: []string{"--maxpeers"}}).Describe())
	assert.Equal(t, "flags ---maxpeers", (&Entry{Remove: []string{"--maxpeers"}}).Describe())
}
