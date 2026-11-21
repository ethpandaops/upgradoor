package host

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/host/system"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
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

func TestValidatorsConfigValidate(t *testing.T) {
	for _, good := range []Validators{
		{},
		{Start: 0, End: 64},
		{MarkerFile: "/data/validator/.validator_keyrange"},
		{MarkerFile: "/data/validator/.validator_keyrange", Start: 0, End: 64},
	} {
		cfg := Config{Validators: good}
		require.NoError(t, cfg.Validate(), "%+v", good)
	}

	for _, bad := range []Validators{
		{Start: 64, End: 0},
		{Start: 64, End: 64},
		{Start: 0, End: protocol.MaxHostValidators + 1},
	} {
		cfg := Config{Validators: bad}
		require.Error(t, cfg.Validate(), "%+v", bad)
	}
}
