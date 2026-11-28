package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/lease"
	"github.com/ethpandaops/upgradoor/pkg/agent/link"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/logins"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// writeConfig writes an agent.yaml into the test's directory.
func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestLoadConfigReadsEveryKey(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(filepath.Join("testdata", "agent.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "unix:///run/docker-test.sock", cfg.Host.Docker.Host)
	assert.Equal(t, "/root/.docker/test.json", cfg.Host.Docker.AuthFile)
	assert.Equal(t, map[protocol.ContainerKind]string{
		protocol.KindEL: "http://127.0.0.1:8545", protocol.KindCL: "http://127.0.0.1:5052",
		"snooper-el": "http://127.0.0.1:9000/health",
	}, map[protocol.ContainerKind]string(cfg.Host.Probes))
	assert.Equal(t, []string{"/usr/bin/sudo", "-n", "/usr/local/lib/upgradoor/upgrade-packages"},
		cfg.Host.Commands.UpgradePackages)
	assert.Equal(t, []string{"/usr/bin/sudo", "-n", "/usr/bin/systemctl", "reboot"}, cfg.Host.Commands.Reboot)

	cfg.Host = host.Config{}

	assert.Equal(t, &Config{
		Controller: link.Config{
			URL:          "https://upgradoor.testnet-0.example",
			SyncInterval: 20 * time.Second, LeaseSyncInterval: 3 * time.Second,
			MinSyncInterval: 2 * time.Second, MaxSyncInterval: 5 * time.Minute,
			PublicKey: agenttest.ControllerPublicKey(t),
		},
		KeyFile:  "/etc/upgradoor/test.key",
		CertFile: "/etc/upgradoor/test.crt",
		Roles:    []string{"ethereum_node", "rpc"},
		StateDir: "/data/upgradoor-test",
		Local: local.Config{
			EntryUser:     "ops",
			OverridesFile: "/home/ops/custom-overrides.yaml",
			ManagedFile:   "/etc/upgradoor/test-managed.yaml",
			Socket:        local.Socket{Path: "/run/upgradoor/test.sock", Group: "ops"},
			AuditLog: local.AuditConfig{
				File: "/var/log/upgradoor/audit.log", MaxSizeBytes: 2 << 20, MaxFiles: 3,
			},
		},
		Policy: plan.Policy{
			Verbs:         []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot},
			DefaultPolicy: plan.DefaultRoll,
			KindPolicies: map[protocol.ContainerKind]plan.KindPolicy{
				protocol.KindEL: {Roll: true}, "snooper-el": {Roll: false},
			},
			Packages: plan.Packages{Blocked: []string{"docker-ce", "containerd.io"}},
			Fallback: plan.Fallback{After: 2 * time.Hour, Interval: 20 * time.Minute},
		},
		Verify: lease.Config{Window: 30 * time.Second, Timeout: 5 * time.Minute},
		SSHLogins: logins.Config{
			Enabled: true, Backlog: 48 * time.Hour, KeyMap: "/etc/upgradoor/test-ssh-keys.json",
		},
	}, cfg)
}

func TestLoadConfigAppliesTheDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t,
		"controller:\n  url: https://upgradoor.example\n  allowUnsignedAnswers: true\n"))
	require.NoError(t, err)

	assert.True(t, cfg.defaulted())
	assert.Equal(t, "/etc/upgradoor/host.key", cfg.KeyFile)
	assert.Equal(t, "/etc/upgradoor/host.crt", cfg.CertFile)
	assert.Equal(t, "/data/upgradoor", cfg.StateDir)
	assert.Equal(t, 30*time.Second, cfg.Controller.SyncInterval)
	assert.Equal(t, local.DefaultSocket, cfg.Local.Socket.Path)
	assert.Equal(t, "/data/upgradoor/audit.log", cfg.Local.AuditLog.File)
	assert.Equal(t, "unix:///var/run/docker.sock", cfg.Host.Docker.Host)
	assert.Equal(t, "/root/.docker/config.json", cfg.Host.Docker.AuthFile)
	assert.Equal(t, plan.DefaultIgnore, cfg.Policy.DefaultPolicy)
	assert.Equal(t, 45*time.Second, cfg.Verify.Window)
	assert.False(t, cfg.SSHLogins.Enabled, "the SSH login history is off by default")
	assert.Equal(t, 168*time.Hour, cfg.SSHLogins.Backlog)
	assert.Equal(t, "/etc/upgradoor/ssh-keys.json", cfg.SSHLogins.KeyMap)
}

func TestLoadConfigRefusesWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()

	head := "controller:\n  url: https://upgradoor.example\n  allowUnsignedAnswers: true\n"

	for name, tt := range map[string]struct {
		yaml string
		want string
	}{
		"an unknown top-level key":    {head + "containerz: {}\n", "not found"},
		"an unknown controller key":   {head + "  pubKeys: []\n", "not found"},
		"an unknown key of an inline": {head + "socket:\n  mode: 0777\n", "not found"},
		"an unknown kind policy key":  {head + "kindPolicies:\n  el: {rolls: true}\n", "not found"},
		"an unknown docker key":       {head + "docker:\n  auth: x\n", "not found"},
		"a probe of no kind":          {head + "probes:\n  EL: http://a\n", `"EL" is not a kind`},
		"groups":                      {head + "groups: {}\n", "groups: no longer supported: containers are configured by their labels"},
		"the name map":                {head + "containers: {}\n", "containers: no longer supported"},
		"defaultGroup":                {head + "defaultGroup:\n  roll: true\n", "defaultGroup: no longer supported: defaultPolicy"},
		"validators":                  {head + "validators: {}\n", "validators: no longer supported"},
		"registry":                    {head + "registry:\n  authFile: x\n", "registry: no longer supported: the pull credentials are docker.authFile"},
		"fallback.containers":         {head + "fallback:\n  containers: [geth]\n", "fallback.containers: no longer supported"},
		"an unknown verify key":       {head + "verify:\n  grace: 1s\n", "not found"},
		"no controller url":           {"controller:\n  allowUnsignedAnswers: true\n", "absolute URL"},
		"no controller key":           {"controller:\n  url: https://upgradoor.example\n", "publicKey is required"},
		"an unknown verb":             {head + "verbs: [exec]\n", `unknown verb "exec"`},
		"not yaml":                    {"controller: [\n", "parse config"},
		"a role that is no name":      {head + "roles: [a/b]\n", "roles"},
		"a relative key map":          {head + "sshLogins:\n  keyMap: keys.json\n", "sshLogins.keyMap"},
		"an unknown sshLogins key":    {head + "sshLogins:\n  users: [devops]\n", "not found"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, tt.yaml))
			require.ErrorContains(t, err, tt.want)
		})
	}

	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorContains(t, err, "read config")
}
