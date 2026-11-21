package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestHolds(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{HoldFile: filepath.Join(dir, "etc", "hold"), StateDir: filepath.Join(dir, "state")}
	h := newHolds(cfg)

	assert.Equal(t, protocol.Hold{}, h.Read())

	// The controller hold is a flag in the state directory.
	require.NoError(t, h.SetController("maintenance\nwindow"))
	assert.Equal(t, protocol.Hold{Controller: true}, h.Read())
	assert.FileExists(t, filepath.Join(cfg.StateDir, controllerHoldFile))

	require.NoError(t, h.SetController(""), "setting it twice is fine")
	assert.True(t, newHolds(cfg).Read().Controller, "it is on disk")

	// The local hold is a file a person creates.
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.HoldFile), 0o700))
	require.NoError(t, os.WriteFile(cfg.HoldFile, []byte("  debugging\tteku  \nmore\n"), 0o600))
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging teku", Controller: true}, h.Read())

	// Releasing the controller hold leaves the local one alone.
	require.NoError(t, h.ReleaseController())
	require.NoError(t, h.ReleaseController(), "releasing twice is fine")
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging teku"}, h.Read())
	assert.FileExists(t, cfg.HoldFile)

	// An empty hold file holds too, without a reason.
	require.NoError(t, os.WriteFile(cfg.HoldFile, nil, 0o600))
	assert.Equal(t, protocol.Hold{Local: true}, h.Read())

	// So does anything else at that path: when in doubt, hold.
	require.NoError(t, os.Remove(cfg.HoldFile))
	require.NoError(t, os.Mkdir(cfg.HoldFile, 0o700))
	assert.True(t, h.Read().Local)

	require.NoError(t, os.Remove(cfg.HoldFile))
	assert.False(t, h.Read().Active())

	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}

	require.NoError(t, os.WriteFile(cfg.HoldFile, long, 0o600))
	assert.Len(t, h.Read().LocalReason, maxHoldReason, "the reported reason is bounded")
}
