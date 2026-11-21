package config

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestValidatorsConfigValidate(t *testing.T) {
	for _, good := range []Validators{
		{},
		{Start: 0, End: 64},
		{MarkerFile: "/data/validator/.validator_keyrange"},
		{MarkerFile: "/data/validator/.validator_keyrange", Start: 0, End: 64},
	} {
		require.NoError(t, good.validate(), "%+v", good)
	}

	for _, bad := range []Validators{
		{Start: 64, End: 0},
		{Start: 64, End: 64},
		{Start: 0, End: protocol.MaxHostValidators + 1},
	} {
		require.Error(t, bad.validate(), "%+v", bad)
	}
}
