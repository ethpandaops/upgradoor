package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestValidatorsReport(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".validator_keyrange")

	write := func(t *testing.T, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(marker, []byte(content), 0o600))
	}

	known := func(start, end uint64, source string) *protocol.Validators {
		return &protocol.Validators{
			Start: start, End: end, Source: source, State: protocol.ValidatorsKnown,
		}
	}

	t.Run("nothing configured: no validators", func(t *testing.T) {
		assert.Nil(t, (&ValidatorsConfig{}).report())
	})

	t.Run("the static range, without a marker file", func(t *testing.T) {
		cfg := &ValidatorsConfig{Start: 100, End: 164}
		assert.Equal(t, known(100, 164, protocol.ValidatorsFromConfig), cfg.report())
	})

	// With a marker file configured the static range is never used: not for
	// a marker that is missing, and not for one that is damaged.
	cfg := &ValidatorsConfig{MarkerFile: marker, Start: 100, End: 164}

	t.Run("the marker is read again for every report", func(t *testing.T) {
		write(t, "0-64\n")
		assert.Equal(t, known(0, 64, protocol.ValidatorsFromMarker), cfg.report())

		write(t, "32-96\n")
		assert.Equal(t, known(32, 96, protocol.ValidatorsFromMarker), cfg.report())
	})

	t.Run("0-0 is a statement: no validators", func(t *testing.T) {
		write(t, "0-0\n")

		got := cfg.report()
		assert.Equal(t, known(0, 0, protocol.ValidatorsFromMarker), got)
		assert.Zero(t, got.Count())
	})

	t.Run("a missing marker is not none", func(t *testing.T) {
		require.NoError(t, os.Remove(marker))

		got := cfg.report()
		assert.Equal(t, &protocol.Validators{
			Source: protocol.ValidatorsFromMarker, State: protocol.ValidatorsMissing,
		}, got)
		assert.False(t, got.Known())
	})

	for name, content := range map[string]string{
		"garbage":           "all of them\n",
		"empty":             "",
		"reversed":          "64-0\n",
		"one number":        "64\n",
		"two lines":         "0-64\n64-128\n",
		"longer than a key": strings.Repeat("9", 4096),
	} {
		t.Run("a marker that cannot be parsed is an error, not none: "+name, func(t *testing.T) {
			write(t, content)

			got := cfg.report()
			assert.Equal(t, protocol.ValidatorsInvalid, got.State)
			assert.Equal(t, protocol.ValidatorsFromMarker, got.Source)
			assert.NotEmpty(t, got.Error)
			assert.Zero(t, got.Count())
			require.NoError(t, got.Validate(), "the controller accepts the report as it is")
		})
	}

	t.Run("a marker that cannot be read is an error, not none", func(t *testing.T) {
		// A directory in its place.
		broken := &ValidatorsConfig{MarkerFile: dir}

		got := broken.report()
		assert.Equal(t, protocol.ValidatorsInvalid, got.State)
		assert.NotEmpty(t, got.Error)
	})
}

func TestValidatorsConfigValidate(t *testing.T) {
	for _, good := range []ValidatorsConfig{
		{},
		{Start: 0, End: 64},
		{MarkerFile: "/data/validator/.validator_keyrange"},
		{MarkerFile: "/data/validator/.validator_keyrange", Start: 0, End: 64},
	} {
		require.NoError(t, good.validate(), "%+v", good)
	}

	for _, bad := range []ValidatorsConfig{
		{Start: 64, End: 0},
		{Start: 64, End: 64},
		{Start: 0, End: protocol.MaxHostValidators + 1},
	} {
		require.Error(t, bad.validate(), "%+v", bad)
	}
}
