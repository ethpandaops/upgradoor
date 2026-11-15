package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// TestSyncReportsOwners: the owners of the agent configuration go out with
// each managed container, sorted; a managed container without owners and an
// unmanaged container carry none.
func TestSyncReportsOwners(t *testing.T) {
	e := newEnv(t, baseScenario())
	e.cfg.Containers[0].Owners = []string{"panda", "geth"}
	e.cfg.Containers[1].Owners = []string{"teku"}
	require.NoError(t, e.cfg.Validate())

	e.start()

	req := e.ctrl.await("first sync", func(req *protocol.SyncRequest) bool {
		return len(req.Containers) == 4
	})

	assert.Equal(t, []string{"geth", "panda"}, containerOf(t, &req, "geth").Owners)
	assert.Equal(t, []string{"teku"}, containerOf(t, &req, "teku").Owners)
	assert.Nil(t, containerOf(t, &req, "validator").Owners, "managed, nobody owns it")
	assert.Nil(t, containerOf(t, &req, "dora").Owners, "not managed")
}

func TestConfigOwners(t *testing.T) {
	config := func(owners string) string {
		return "controller:\n  url: https://upgradoor.example\n" +
			"containers:\n  - name: geth\n    kind: el\n    image: ethpandaops/geth:master\n" + owners
	}

	load := func(t *testing.T, content string) (*agent.Config, error) {
		t.Helper()

		path := filepath.Join(t.TempDir(), "agent.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		return agent.LoadConfig(path)
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
