package host

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/host/system"
)

func TestApplyDefaults(t *testing.T) {
	var cfg Config

	cfg.ApplyDefaults()
	assert.Equal(t, "unix:///var/run/docker.sock", cfg.Docker.Host)

	cfg.Docker.Host = "tcp://127.0.0.1:2375"
	cfg.ApplyDefaults()
	assert.Equal(t, "tcp://127.0.0.1:2375", cfg.Docker.Host, "a set value stays")
}

func TestCommandsValidate(t *testing.T) {
	cfg := Config{Commands: system.Commands{
		UpgradePackages: []string{"/usr/bin/sudo", "-n", "/usr/local/lib/upgradoor/upgrade-packages"},
	}}
	require.NoError(t, cfg.Validate())

	cfg.Commands.Reboot = []string{"sudo", "reboot"}
	require.ErrorContains(t, cfg.Validate(), "commands: reboot")
}
