package overrides

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestKeeperHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.yaml")
	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)

	h := newKeeper(nil, &config.Config{OverridesFile: path}, nil, func() time.Time { return now }, nil, nil)

	assert.Equal(t, protocol.Hold{}, h.Hold(), "no file is no hold")

	// The controller hold is an entry of the overrides file, with who asked
	// as the controller says.
	sam := &protocol.Actor{User: "sam@example.org", Teams: []string{"lighthouse"}, Feature: "team_hold"}
	require.NoError(t, h.SetControllerHold("maintenance\nwindow", sam))
	assert.Equal(t, protocol.Hold{Controller: true}, h.Hold())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "hold:\n  # [hold:controller] 2026-11-20T16:00:00Z controller:sam@example.org: set: maintenance\n"+
		"  controller:\n    by: \"sam@example.org\"\n    reason: \"maintenance\"\n    teams: [\"lighthouse\"]\n"+
		"    feature: \"team_hold\"\n")

	// The local hold, by hand, next to it.
	require.NoError(t, os.WriteFile(path, append(raw, []byte("  local:\n    by: pk\n    reason: debugging teku\n")...), 0o600))
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging teku", Controller: true}, h.Hold())

	// Releasing the controller hold leaves the local one alone.
	require.NoError(t, h.ReleaseControllerHold())
	require.NoError(t, h.ReleaseControllerHold(), "releasing twice is fine")
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging teku"}, h.Hold())

	// A local hold whose end has passed holds no more.
	require.NoError(t, h.SetLocalHold("pk", "until noon"))
	assert.True(t, h.Hold().Local)

	raw, err = os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw),
		"reason: \"until noon\"\n", "reason: \"until noon\"\n    until: 2026-11-20T12:00:00Z\n", 1)), 0o600))
	assert.False(t, h.Hold().Active())

	require.NoError(t, h.ReleaseLocalHold("pk"))
	assert.False(t, h.Hold().Active())

	// A file that cannot be read holds: when in doubt, hold.
	require.NoError(t, os.WriteFile(path, []byte("hold: [broken\n"), 0o600))
	hold := h.Hold()
	assert.True(t, hold.Local)
	assert.Contains(t, hold.LocalReason, "cannot be read")

	require.NoError(t, os.WriteFile(path, []byte("hold:\n  somebody: {reason: x}\n"), 0o600))
	assert.True(t, h.Hold().Local, "an unknown hold source is a broken file")
}
