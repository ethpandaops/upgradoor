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
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/agent/registry"
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
	assert.Equal(t, "http://127.0.0.1:8545", cfg.Host.Probes.EL)
	assert.Equal(t, "http://127.0.0.1:5052", cfg.Host.Probes.CL)
	assert.Equal(t, host.Validators{MarkerFile: "/data/.validator_keyrange"}, cfg.Host.Validators)

	cfg.Host = host.Config{}

	assert.Equal(t, &Config{
		Controller: link.Config{
			URL:          "https://upgradoor.testnet-0.example",
			SyncInterval: 20 * time.Second, LeaseSyncInterval: 3 * time.Second,
			MinSyncInterval: 2 * time.Second, MaxSyncInterval: 5 * time.Minute,
			PublicKeys: []string{agenttest.ControllerPublicKey(t, 0)},
		},
		KeyFile:  "/etc/upgradoor/test.key",
		CertFile: "/etc/upgradoor/test.crt",
		StateDir: "/var/lib/upgradoor-test",
		Local: local.Config{
			EntryUser:     "ops",
			OverridesFile: "/home/ops/custom-overrides.yaml",
			Socket:        local.Socket{Path: "/run/upgradoor/test.sock", Group: "ops"},
			AuditLog: local.AuditConfig{
				File: "/var/log/upgradoor/audit.log", MaxSizeBytes: 2 << 20, MaxFiles: 3,
			},
		},
		Policy: plan.Policy{
			Verbs: []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot},
			Groups: map[string]plan.Group{
				"execution": {
					Client: "geth", Owners: []string{"geth", "panda"},
					Overrides: []string{protocol.OverrideImage, protocol.OverrideFlags},
					Impact:    protocol.ImpactNode, StopTimeout: 10 * time.Minute,
				},
				"tooling": {Impact: protocol.ImpactIndependent},
			},
			Containers: map[string]plan.MappedContainer{
				"execution": {Group: "execution", Kind: protocol.KindEL},
				"exporter":  {Group: "tooling", Kind: protocol.KindOther},
			},
			DefaultGroup: plan.DefaultGroup{Roll: true, Exclude: []string{"tmp-*", "debug"}},
			Packages:     plan.Packages{Blocked: []string{"docker-ce", "containerd.io"}},
		},
		Registry: registry.Config{
			PollInterval: 15 * time.Minute, AuthFile: "/root/.docker/test.json",
			PlainHTTP: []string{"registry.local:5000"}, CAFile: "/etc/ssl/registry-ca.pem",
		},
		Verify: lease.Config{Window: 30 * time.Second, Timeout: 5 * time.Minute},
	}, cfg)
}

func TestLoadConfigAppliesTheDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t,
		"controller:\n  url: https://upgradoor.example\n  allowUnsignedAnswers: true\n"))
	require.NoError(t, err)

	assert.True(t, cfg.defaulted())
	assert.Equal(t, DefaultKeyFile, cfg.KeyFile)
	assert.Equal(t, DefaultCertFile, cfg.CertFile)
	assert.Equal(t, DefaultStateDir, cfg.StateDir)
	assert.Equal(t, 30*time.Second, cfg.Controller.SyncInterval)
	assert.Equal(t, local.DefaultSocket, cfg.Local.Socket.Path)
	assert.Equal(t, filepath.Join(DefaultStateDir, "audit.log"), cfg.Local.AuditLog.File)
	assert.Equal(t, "unix:///var/run/docker.sock", cfg.Host.Docker.Host)
	assert.Equal(t, 30*time.Minute, cfg.Registry.PollInterval)
	assert.Equal(t, 45*time.Second, cfg.Verify.Window)
}

func TestLoadConfigRefusesWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()

	head := "controller:\n  url: https://upgradoor.example\n  allowUnsignedAnswers: true\n"

	for name, tt := range map[string]struct {
		yaml string
		want string
	}{
		"an unknown top-level key":     {head + "containerz: {}\n", "not found"},
		"an unknown controller key":    {head + "  pubKeys: []\n", "not found"},
		"an unknown key of an inline":  {head + "socket:\n  mode: 0777\n", "not found"},
		"an image in the name map":     {head + "containers:\n  a: {group: g, image: x}\n", "not found"},
		"an unknown key in a group":    {head + "groups:\n  g: {owner: x}\n", "not found"},
		"an unknown defaultGroup key":  {head + "defaultGroup:\n  rolls: true\n", "not found"},
		"an unknown verify key":        {head + "verify:\n  grace: 1s\n", "not found"},
		"no controller url":            {"controller:\n  allowUnsignedAnswers: true\n", "absolute URL"},
		"no controller key":            {"controller:\n  url: https://upgradoor.example\n", "publicKeys is required"},
		"an unknown verb":              {head + "verbs: [exec]\n", `unknown verb "exec"`},
		"a static range start and end": {head + "validators: {start: 5, end: 5}\n", "validators"},
		"not yaml":                     {"controller: [\n", "parse config"},
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
