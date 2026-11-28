package controller

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func testSignerKey(t *testing.T) string {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)

	return identity.EncodePublicKey(identity.PublicKey(key))
}

// validConfig is the smallest configuration Validate accepts.
func validConfig(t *testing.T) *Config {
	t.Helper()

	cfg := DefaultConfig()
	cfg.Network.Name = "testnet-devnet-0"
	cfg.Network.BeaconEndpoints = []string{"http://beacon:5052"}
	cfg.Agents.SignerPublicKey = testSignerKey(t)
	cfg.Agents.ControllerSeedFile = "/run/secrets/controller.seed"
	cfg.Auth.ProviderURL = "https://auth.example"

	return &cfg
}

func TestLoadConfigDefaults(t *testing.T) {
	want := DefaultConfig()

	assert.Equal(t, ":8080", want.Server.Listen)
	assert.Equal(t, "/data", want.Store.DataDir)
	assert.Equal(t, 24*time.Hour, want.DeployWindow.Std())
	assert.Equal(t, time.Minute, want.Images.SourceInterval.Std())
	assert.Equal(t, "upgradoor", want.Auth.Service)
	assert.Zero(t, want.Store.AuditRetention)
	assert.False(t, want.UI.Pprof)

	for _, path := range []string{"", writeConfig(t, ""), writeConfig(t, "# nothing\n")} {
		cfg, err := LoadConfig(path, nil)
		require.NoError(t, err, path)
		assert.Equal(t, &want, cfg, path)
	}

	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml"), nil)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestLoadConfigFile(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
server:
  listen: 127.0.0.1:9000
  publicUrls: [https://upgradoor.example]
network:
  name: testnet-devnet-0
  beaconEndpoints:
    - http://a:5052
    - http://b:5052
deployWindow: 0s
agents:
  allowUnsignedAnswers: true
auth:
  allowOpenApi: true
images:
  registryPlainHttp: [localhost:5000]
  sources: [ethpandaops/grandine=grandinetech/grandine]
settingDefaults:
  leaseTtl: 5m
  maxUnavailablePercent: 25
  policyKinds: {el: manual}
store:
  auditRetention: 720h
`), nil)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1:9000", cfg.Server.Listen)
	assert.Equal(t, "/data", cfg.Store.DataDir, "a key the file leaves out keeps its default")
	assert.Zero(t, cfg.DeployWindow)
	assert.True(t, cfg.Agents.AllowUnsignedAnswers)
	assert.Equal(t, []string{"https://upgradoor.example"}, cfg.Server.PublicURLs)
	assert.Equal(t, []string{"http://a:5052", "http://b:5052"}, cfg.Network.BeaconEndpoints)
	assert.Equal(t, []string{"localhost:5000"}, cfg.Images.RegistryPlainHTTP)
	assert.Equal(t, 720*time.Hour, cfg.Store.AuditRetention.Std())

	settings, err := cfg.Settings()
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, settings.LeaseTTL.Std())
	assert.InDelta(t, 25.0, settings.MaxUnavailablePercent, 0)
	assert.Equal(t, fleet.KindPolicies{"el": fleet.PolicyManual}, settings.PolicyKinds)
}

// Unknown keys, in the file or in --set, are errors; so is a duration
// without a unit.
func TestLoadConfigStrict(t *testing.T) {
	for _, content := range []string{
		"server:\n  listen: :8080\n  listn: :9090\n",
		"auth:\n  provider: https://auth.example\n",
		"listen: :8080\n",
		"pprof:\n  enabled: true\n",
		"deployWindow: 30\n",
		"server:\n  listen: [a]\n",
		"- a\n",
		"server: {listen: a}\nserver: {listen: b}\n",
	} {
		_, err := LoadConfig(writeConfig(t, content), nil)
		require.Error(t, err, content)
	}

	for _, set := range []string{
		"nokey", "=1", "listen=:9090", "auth.provider=x", "auth=x", "server.listen.x=1", "settingDefaults.a.b=1",
		"deployWindow=30", "agents.allowInsecureHttp=maybe",
	} {
		_, err := LoadConfig("", []string{set})
		require.Error(t, err, set)
	}
}

func TestLoadConfigSet(t *testing.T) {
	path := writeConfig(t, "network:\n  name: from-file\n  beaconEndpoints: [http://file:5052]\nauth:\n  service: other\n")

	cfg, err := LoadConfig(path, []string{
		"network.name=testnet-devnet-0",
		"server.listen=127.0.0.1:8099",
		"deployWindow=0",
		"images.sourceInterval=90s",
		"agents.allowInsecureHttp=true",
		"auth.providerUrl=http://127.0.0.1:18099",
		"network.beaconEndpoints=http://a:4000, http://b:4000",
		"images.registryPlainHttp=localhost:5055",
		"ui.origins=[https://x.example]",
		"ui.pprof=true",
		"server.publicUrls=",
		"settingDefaults.leaseTtl=5m",
		"settingDefaults.readyAttestations=2",
		"settingDefaults.paused=true",
		`settingDefaults.policyKinds={"cl": "auto"}`,
		"store.auditRetention=48h",
		"network.name=testnet-devnet-1",
	})
	require.NoError(t, err)

	assert.Equal(t, "testnet-devnet-1", cfg.Network.Name, "the last --set wins")
	assert.Equal(t, "other", cfg.Auth.Service, "the file stays where --set does not reach")
	assert.Equal(t, "127.0.0.1:8099", cfg.Server.Listen)
	assert.Zero(t, cfg.DeployWindow)
	assert.Equal(t, 90*time.Second, cfg.Images.SourceInterval.Std())
	assert.True(t, cfg.Agents.AllowInsecureHTTP)
	assert.Equal(t, "http://127.0.0.1:18099", cfg.Auth.ProviderURL)
	assert.Equal(t, []string{"http://a:4000", "http://b:4000"}, cfg.Network.BeaconEndpoints)
	assert.Equal(t, []string{"localhost:5055"}, cfg.Images.RegistryPlainHTTP)
	assert.Equal(t, []string{"https://x.example"}, cfg.UI.Origins)
	assert.True(t, cfg.UI.Pprof)
	assert.Empty(t, cfg.Server.PublicURLs)
	assert.Equal(t, 48*time.Hour, cfg.Store.AuditRetention.Std())

	settings, err := cfg.Settings()
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, settings.LeaseTTL.Std())
	assert.Equal(t, uint64(2), settings.ReadyAttestations)
	assert.True(t, settings.Paused)
	assert.Equal(t, fleet.KindPolicies{"cl": fleet.PolicyAuto}, settings.PolicyKinds)
}

func TestConfigSettings(t *testing.T) {
	cfg := DefaultConfig()

	s, err := cfg.Settings()
	require.NoError(t, err)
	assert.Equal(t, fleet.DefaultSettings(), s)

	cfg.SettingDefaults = map[string]any{
		"maxUnavailablePercent": 25, "leaseTtl": "10m", "policyReboot": "auto", "readyAttestations": 3,
		"readyStableFor": "5m", "minPeers": 5, "paused": true, "policyImage": "manual", "readyPeerShare": 0.25,
		"pauseAfterCanaryImage": true,
		"policyClients":         map[string]any{"cl": map[string]any{"lighthouse": "auto"}},
		"actionPolicies":        map[string]any{"security-upgrades": "auto"},
		"policyKinds":           map[string]any{"el": "manual", "snooper-el": "auto", "other": "manual"},
	}

	s, err = cfg.Settings()
	require.NoError(t, err)
	assert.InDelta(t, 25.0, s.MaxUnavailablePercent, 0)
	assert.Equal(t, 10*time.Minute, s.LeaseTTL.Std())
	assert.Equal(t, fleet.PolicyAuto, s.PolicyReboot)
	assert.Equal(t, fleet.PolicyManual, s.PolicyImage)
	assert.Equal(t, uint64(3), s.ReadyAttestations)
	assert.Equal(t, 5*time.Minute, s.ReadyStableFor.Std())
	assert.Equal(t, 5, s.MinPeers)
	assert.True(t, s.Paused)
	assert.InDelta(t, 0.25, s.ReadyPeerShare, 0)
	assert.True(t, s.PauseAfterCanaryImage)
	assert.Equal(t, fleet.ClientPolicies{"cl": {"lighthouse": fleet.PolicyAuto}}, s.PolicyClients)
	assert.Equal(t, fleet.ActionPolicies{"security-upgrades": fleet.PolicyAuto}, s.ActionPolicies)
	assert.Equal(t, fleet.KindPolicies{
		"el": fleet.PolicyManual, "snooper-el": fleet.PolicyAuto, fleet.KindOther: fleet.PolicyManual,
	}, s.PolicyKinds)

	for key, bad := range map[string]any{
		"unknown": 1, "leaseTtl": 30, "maxUnavailablePercent": 200, "policyImage": "sometimes",
		"readyAttestations": "many", "minPeers": 0, "readyPeerShare": 2, "pauseAfterCanaryImage": "sometimes",
		"policyKinds": []any{"cl"},
	} {
		cfg.SettingDefaults = map[string]any{key: bad}
		_, err := cfg.Settings()
		require.Error(t, err, key)

		valid := validConfig(t)
		valid.SettingDefaults = cfg.SettingDefaults
		require.ErrorContains(t, valid.Validate(), "settingDefaults: ", key)
	}
}

func TestConfigValidate(t *testing.T) {
	require.NoError(t, validConfig(t).Validate())

	open := validConfig(t)
	open.Auth = AuthConfig{AllowOpenAPI: true, Service: "upgradoor"}
	open.Agents.ControllerSeedFile = ""
	open.Agents.AllowUnsignedAnswers = true
	require.NoError(t, open.Validate())

	profiled := validConfig(t)
	profiled.UI.Pprof = true
	require.NoError(t, profiled.Validate())

	for name, tc := range map[string]struct {
		change func(c *Config)
		key    string
	}{
		"no network":         {func(c *Config) { c.Network.Name = "" }, "network.name: is required"},
		"bad network":        {func(c *Config) { c.Network.Name = "Not A Name" }, "network.name: "},
		"no listen":          {func(c *Config) { c.Server.Listen = "" }, "server.listen: is required"},
		"no data dir":        {func(c *Config) { c.Store.DataDir = "" }, "store.dataDir: is required"},
		"no signer":          {func(c *Config) { c.Agents.SignerPublicKey = "" }, "agents.signerPublicKey: "},
		"no seed":            {func(c *Config) { c.Agents.ControllerSeedFile = "" }, "agents.controllerSeedFile: is required"},
		"seed and unsigned":  {func(c *Config) { c.Agents.AllowUnsignedAnswers = true }, "excludes agents.allowUnsignedAnswers"},
		"no auth":            {func(c *Config) { c.Auth.ProviderURL = "" }, "auth.providerUrl: is required"},
		"auth and open":      {func(c *Config) { c.Auth.AllowOpenAPI = true }, "excludes auth.allowOpenApi"},
		"no beacon":          {func(c *Config) { c.Network.BeaconEndpoints = nil }, "network.beaconEndpoints: "},
		"bad beacon":         {func(c *Config) { c.Network.BeaconEndpoints = []string{"beacon"} }, "network.beaconEndpoints[0]: "},
		"negative window":    {func(c *Config) { c.DeployWindow = -1 }, "deployWindow: "},
		"long window":        {func(c *Config) { c.DeployWindow = Duration(721 * time.Hour) }, "deployWindow: "},
		"no source interval": {func(c *Config) { c.Images.SourceInterval = 0 }, "images.sourceInterval: "},
		"bad image source":   {func(c *Config) { c.Images.Sources = []string{"x"} }, "images.sources[0]: "},
		"negative retention": {func(c *Config) { c.Store.AuditRetention = -1 }, "store.auditRetention: "},
		"short retention":    {func(c *Config) { c.Store.AuditRetention = Duration(time.Minute) }, "store.auditRetention: "},
		"pprof open": {func(c *Config) {
			c.Auth = AuthConfig{AllowOpenAPI: true}
			c.UI.Pprof = true
		}, "ui.pprof: needs auth.providerUrl"},
	} {
		cfg := validConfig(t)
		tc.change(cfg)
		require.ErrorContains(t, cfg.Validate(), tc.key, name)
	}

	// Every problem is named at once.
	err := (&Config{}).Validate()
	require.Error(t, err)

	for _, key := range []string{
		"server.listen", "store.dataDir", "network.name", "network.beaconEndpoints", "agents.signerPublicKey",
		"auth.providerUrl",
	} {
		assert.ErrorContains(t, err, key+": ")
	}
}
