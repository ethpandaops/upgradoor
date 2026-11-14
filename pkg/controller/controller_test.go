package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

func TestParseDefaultSettings(t *testing.T) {
	s, err := ParseDefaultSettings(nil)
	require.NoError(t, err)
	assert.Equal(t, fleet.DefaultSettings(), s)

	s, err = ParseDefaultSettings([]string{
		"maxUnavailablePercent=25", "leaseTtl=10m", "policyReboot=auto",
		"soakEpochs=2", "paused=true", `policyImage="manual"`,
	})
	require.NoError(t, err)
	assert.InDelta(t, 25.0, s.MaxUnavailablePercent, 0)
	assert.Equal(t, 10*time.Minute, s.LeaseTTL.Std())
	assert.Equal(t, fleet.PolicyAuto, s.PolicyReboot)
	assert.Equal(t, fleet.PolicyManual, s.PolicyImage)
	assert.Equal(t, uint64(2), s.SoakEpochs)
	assert.True(t, s.Paused)

	for _, bad := range []string{
		"nokey", "=1", "unknown=1", "leaseTtl=30", "maxUnavailablePercent=200",
		"policyImage=sometimes", "soakEpochs=many",
	} {
		_, err := ParseDefaultSettings([]string{bad})
		require.Error(t, err, bad)
	}
}
