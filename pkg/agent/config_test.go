package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

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
			cfg := Config{Controller: ControllerConfig{
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

func TestConfigOwners(t *testing.T) {
	config := func(owners string) string {
		return "controller:\n  url: https://upgradoor.example\n  allowUnsignedAnswers: true\n" +
			"containers:\n  - name: geth\n    kind: el\n    image: ethpandaops/geth:master\n" + owners
	}

	load := func(t *testing.T, content string) (*Config, error) {
		t.Helper()

		path := filepath.Join(t.TempDir(), "agent.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		return LoadConfig(path)
	}

	cfg, err := load(t, config(""))
	require.NoError(t, err)
	assert.Empty(t, cfg.Containers[0].Owners, "owners are optional")

	cfg, err = load(t, config("    owners: [geth, panda, team-1, a.b_c]\n"))
	require.NoError(t, err)
	assert.Equal(t, []string{"geth", "panda", "team-1", "a.b_c"}, cfg.Containers[0].Owners)

	eight := "    owners: [a, b, c, d, e, f, g, h]\n"
	_, err = load(t, config(eight))
	require.NoError(t, err, "eight owners are allowed")

	for name, tt := range map[string]struct {
		owners string
		want   string
	}{
		"upper case":   {"[Geth]", `owner "Geth" is not a team name`},
		"blank inside": {`["ge th"]`, `owner "ge th" is not a team name`},
		"empty":        {`[""]`, `owner "" is not a team name`},
		"leading dash": {`["-geth"]`, `owner "-geth" is not a team name`},
		"a grant":      {`["geth:all"]`, `owner "geth:all" is not a team name`},
		"too long":     {"[" + strings.Repeat("a", 64) + "]", "is not a team name"},
		"twice":        {"[geth, panda, geth]", `owner "geth" listed twice`},
		"nine owners":  {"[a, b, c, d, e, f, g, h, i]", "more than 8 owners"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, config("    owners: "+tt.owners+"\n"))
			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, `container "geth"`)
		})
	}
}

func TestConfigClient(t *testing.T) {
	config := func(kind, client string) string {
		return "controller:\n  url: https://upgradoor.example\n  allowUnsignedAnswers: true\n" +
			"containers:\n  - name: beacon\n    kind: " + kind + "\n    image: sigp/lighthouse:latest\n" +
			client
	}

	load := func(t *testing.T, content string) (*Config, error) {
		t.Helper()

		path := filepath.Join(t.TempDir(), "agent.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		return LoadConfig(path)
	}

	cfg, err := load(t, config("cl", ""))
	require.NoError(t, err)
	assert.Empty(t, cfg.Containers[0].Client, "a client is optional")

	for _, kind := range []string{"el", "cl", "vc"} {
		cfg, err = load(t, config(kind, "    client: lighthouse\n"))
		require.NoError(t, err, kind)
		assert.Equal(t, "lighthouse", cfg.Containers[0].Client)
	}

	_, err = load(t, config("cl", "    client: Lighthouse\n"))
	require.ErrorContains(t, err, `client "Lighthouse" is not a client name`)

	_, err = load(t, config("other", "    client: lighthouse\n"))
	require.ErrorContains(t, err, "a client needs the kind el, cl or vc")
}

func TestConfigImpact(t *testing.T) {
	load := func(t *testing.T, kind, extra string) (*Config, error) {
		t.Helper()

		content := "controller:\n  url: https://upgradoor.example\n  allowUnsignedAnswers: true\n" +
			"containers:\n  - name: c\n    kind: " + kind + "\n    image: org/c:latest\n" + extra
		path := filepath.Join(t.TempDir(), "agent.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		return LoadConfig(path)
	}

	for kind, want := range map[string]protocol.Impact{
		"el": protocol.ImpactNode, "cl": protocol.ImpactNode, "vc": protocol.ImpactNode,
		"other": protocol.ImpactIndependent,
	} {
		cfg, err := load(t, kind, "")
		require.NoError(t, err, kind)
		assert.Equal(t, want, cfg.Containers[0].Impact, kind)
	}

	cfg, err := load(t, "other", "    impact: slot\n")
	require.NoError(t, err)
	assert.Equal(t, protocol.ImpactSlot, cfg.Containers[0].Impact)

	_, err = load(t, "el", "    impact: none\n")
	require.ErrorContains(t, err, `unknown impact "none"`)
}
