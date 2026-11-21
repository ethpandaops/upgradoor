package protocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseKeyRange(t *testing.T) {
	good := []struct {
		content    string
		start, end uint64
	}{
		{"0-64", 0, 64},
		{"0-64\n", 0, 64},
		{"  3100-3200 \n", 3100, 3200},
		{"0-0", 0, 0},
		{"7-7\n", 7, 7},
		{"1099511627775-1099511627776", MaxValidatorIndex - 1, MaxValidatorIndex},
	}

	for _, tt := range good {
		start, end, err := ParseKeyRange(tt.content)
		require.NoError(t, err, tt.content)
		assert.Equal(t, tt.start, start, tt.content)
		assert.Equal(t, tt.end, end, tt.content)
	}

	for _, bad := range []string{
		"", "\n", "64", "0-", "-64", "0-64-128", "a-b", "0x0-0x40", "0 - 64", "+0-64",
		"0-64\n0-64", "64-0", "0-1048577", "0-1099511627777",
		"0-99999999999999999999999999", "0..64", "0–64",
	} {
		_, _, err := ParseKeyRange(bad)
		require.Error(t, err, "%q", bad)
	}
}

func TestValidators(t *testing.T) {
	var none *Validators

	assert.False(t, none.Known())
	assert.Zero(t, none.Count())

	known := &Validators{Start: 10, End: 74, Source: ValidatorsFromMarker, State: ValidatorsKnown}
	assert.True(t, known.Known())
	assert.Equal(t, uint64(64), known.Count())
	require.NoError(t, known.Validate())

	empty := &Validators{Source: ValidatorsFromMarker, State: ValidatorsKnown}
	assert.True(t, empty.Known(), "an empty range is a statement: no validators")
	assert.Zero(t, empty.Count())
	require.NoError(t, empty.Validate())

	// A range that is not known counts for nothing, whatever it carries.
	missing := &Validators{Start: 10, End: 74, State: ValidatorsMissing}
	assert.False(t, missing.Known())
	assert.Zero(t, missing.Count())
	require.NoError(t, missing.Validate())

	for _, bad := range []*Validators{
		{Start: 5, End: 4, State: ValidatorsKnown},
		{Start: 0, End: MaxHostValidators + 1, State: ValidatorsKnown},
		{Start: MaxValidatorIndex, End: MaxValidatorIndex + 1, State: ValidatorsKnown},
		{Start: 0, End: 64},
		{Start: 0, End: 64, State: "certain"},
	} {
		require.Error(t, bad.Validate(), "%+v", bad)
	}
}
