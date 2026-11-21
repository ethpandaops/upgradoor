package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// controllerPublicKey is the public key of a test network's controller key of
// a generation, as it stands in controller.publicKeys.
func controllerPublicKey(t *testing.T, generation uint64) string {
	t.Helper()

	seed := []byte("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")

	key, err := identity.DeriveControllerKey(seed, "testnet-0", generation)
	require.NoError(t, err)

	return identity.EncodePublicKey(identity.PublicKey(key))
}

func TestConfigControllerKeys(t *testing.T) {
	key0 := controllerPublicKey(t, 0)
	key1 := controllerPublicKey(t, 1)

	tests := []struct {
		name     string
		keys     []string
		unsigned bool
		want     int
		wantErr  string
	}{
		{name: "one key", keys: []string{key0}, want: 1},
		{name: "two keys for a rotation", keys: []string{key0, key1 + "\n"}, want: 2},
		{name: "the opt-out", unsigned: true, want: 0},
		{name: "nothing configured", wantErr: "controller.publicKeys is required"},
		{
			name: "keys and the opt-out", keys: []string{key0}, unsigned: true,
			wantErr: "exclude each other",
		},
		{name: "not a key", keys: []string{key0, "bm90IGEga2V5"}, wantErr: "publicKeys[1]"},
		{
			name: "a private key in place of a public one",
			keys: []string{"3ccsBTOwGo_w6jKJ-xCQ6L5VPI5GOtA1pIHEY8gpUdA"}, wantErr: "publicKeys[0]",
		},
		{
			name: "too many", keys: []string{key0, key1, key0, key1, key0},
			wantErr: "more than 4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Controller: Controller{
				URL: "https://upgradoor.example", PublicKeys: tt.keys,
				AllowUnsignedAnswers: tt.unsigned,
			}}

			keys, err := cfg.Controller.ControllerKeys()

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.ErrorContains(t, cfg.Validate(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.NoError(t, cfg.Validate())
			assert.Len(t, keys, tt.want)
		})
	}
}

// loadGroups loads an agent.yaml with the given groups block.
func loadGroups(t *testing.T, groups string) (*Config, error) {
	t.Helper()

	content := "controller:\n  url: https://upgradoor.example\n  allowUnsignedAnswers: true\n" + groups
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return Load(path)
}

func TestConfigGroups(t *testing.T) {
	cfg, err := loadGroups(t, `groups:
  consensus:
    client: lighthouse
    owners: [lighthouse, panda]
    overrides: [image, flags]
  proxy:
    impact: independent
    stopTimeout: 30s
`)
	require.NoError(t, err)

	consensus := cfg.Group("consensus")
	assert.Equal(t, "lighthouse", consensus.Client)
	assert.Equal(t, []string{"lighthouse", "panda"}, consensus.Owners)
	assert.Equal(t, &protocol.Overridable{Image: true, Flags: true}, consensus.Overridable())

	proxy := cfg.Group("proxy")
	assert.Equal(t, protocol.ImpactIndependent, proxy.Impact)
	assert.Nil(t, proxy.Overridable(), "a group without overrides is tooling")
	assert.Equal(t, 30*time.Second, proxy.StopTimeout)

	assert.Equal(t, Group{}, cfg.Group("monitoring"), "a group without an entry is admin-only")

	eight := "groups:\n  g:\n    owners: [a, b, c, d, e, f, g, h]\n"
	_, err = loadGroups(t, eight)
	require.NoError(t, err, "eight owners are allowed")

	for name, tt := range map[string]struct {
		groups string
		want   string
	}{
		"a group name in upper case": {"groups:\n  Consensus: {}\n", "not a group name"},
		"an owner in upper case":     {"groups:\n  g:\n    owners: [Geth]\n", `owner "Geth" is not a team name`},
		"a grant as owner":           {"groups:\n  g:\n    owners: [\"geth:all\"]\n", "is not a team name"},
		"an owner twice":             {"groups:\n  g:\n    owners: [geth, geth]\n", `owner "geth" listed twice`},
		"nine owners":                {"groups:\n  g:\n    owners: [a, b, c, d, e, f, g, h, i]\n", "more than 8 owners"},
		"a client in upper case":     {"groups:\n  g:\n    client: Lighthouse\n", `client "Lighthouse" is not a client name`},
		"an unknown impact":          {"groups:\n  g:\n    impact: none\n", `unknown impact "none"`},
		"an unknown part":            {"groups:\n  g:\n    overrides: [env]\n", `unknown part "env"`},
		"a part twice":               {"groups:\n  g:\n    overrides: [image, image]\n", `"image" listed twice`},
		"a long name": {
			"groups:\n  " + strings.Repeat("a", 64) + ": {}\n", "not a group name",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, loadErr := loadGroups(t, tt.groups)
			require.ErrorContains(t, loadErr, tt.want)
		})
	}

	_, err = loadGroups(t, "containers: []\n")
	require.Error(t, err, "the name map is a map")
}

func TestConfigContainersAndDefaultGroup(t *testing.T) {
	cfg, err := loadGroups(t, `containers:
  execution: {group: execution, kind: el}
  beacon: {group: consensus, kind: cl}
  exporter: {group: consensus}
defaultGroup:
  exclude: [tmp-*, debug]
`)
	require.NoError(t, err)

	assert.Equal(t, map[string]MappedContainer{
		"execution": {Group: "execution", Kind: protocol.KindEL},
		"beacon":    {Group: "consensus", Kind: protocol.KindCL},
		"exporter":  {Group: "consensus", Kind: protocol.KindOther},
	}, cfg.Containers)
	assert.False(t, cfg.DefaultGroup.Roll, "the default group is not rolled unless asked")
	assert.True(t, cfg.DefaultGroup.Excludes("tmp-1"))
	assert.True(t, cfg.DefaultGroup.Excludes("debug"))
	assert.False(t, cfg.DefaultGroup.Excludes("debugger"))

	for name, tt := range map[string]struct {
		yaml string
		want string
	}{
		"the default group configured":   {"groups:\n  default: {}\n", "reserved"},
		"a container mapped to none":     {"containers:\n  a: {group: none}\n", "reserved"},
		"a container without a group":    {"containers:\n  a: {kind: el}\n", "not a group name"},
		"an unknown kind":                {"containers:\n  a: {group: g, kind: db}\n", `unknown kind "db"`},
		"an image in the name map":       {"containers:\n  a: {group: g, image: x}\n", "not found"},
		"not a container name":           {"containers:\n  -a: {group: g}\n", "not a container name"},
		"a broken exclude pattern":       {"defaultGroup:\n  exclude: ['[']\n", "not a name or pattern"},
		"an unknown key in defaultGroup": {"defaultGroup:\n  rolls: true\n", "not found"},
	} {
		t.Run(name, func(t *testing.T) {
			_, loadErr := loadGroups(t, tt.yaml)
			require.ErrorContains(t, loadErr, tt.want)
		})
	}
}
