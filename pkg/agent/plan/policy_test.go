package plan

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// loadPolicy reads the policy keys strictly, then applies defaults and validates.
func loadPolicy(t *testing.T, raw string) (*Policy, error) {
	t.Helper()

	var p Policy

	dec := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.KnownFields(true)

	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	p.ApplyDefaults()

	return &p, p.Validate()
}

func TestPolicyGroups(t *testing.T) {
	p, err := loadPolicy(t, `verbs: [roll, reboot]
groups:
  consensus:
    client: lighthouse
    owners: [lighthouse, panda]
    overrides: [image, flags]
  proxy:
    impact: independent
    stopTimeout: 30s
`)
	require.NoError(t, err)

	assert.True(t, p.Allows(protocol.VerbRoll))
	assert.False(t, p.Allows(protocol.VerbPackages))

	consensus := p.group("consensus")
	assert.Equal(t, "lighthouse", consensus.Client)
	assert.Equal(t, []string{"lighthouse", "panda"}, consensus.Owners)
	assert.Equal(t, &protocol.Overridable{Image: true, Flags: true}, consensus.overridable())

	proxy := p.group("proxy")
	assert.Equal(t, protocol.ImpactIndependent, proxy.Impact)
	assert.Nil(t, proxy.overridable(), "a group without overrides is tooling")
	assert.Equal(t, 30*time.Second, proxy.StopTimeout)

	assert.Equal(t, Group{}, p.group("monitoring"), "a group without an entry is admin-only")

	_, err = loadPolicy(t, "groups:\n  g:\n    owners: [a, b, c, d, e, f, g, h]\n")
	require.NoError(t, err, "eight owners are allowed")

	for name, tt := range map[string]struct {
		groups string
		want   string
	}{
		"an unknown verb":            {"verbs: [exec]\n", `unknown verb "exec"`},
		"a group name in upper case": {"groups:\n  Consensus: {}\n", "not a group name"},
		"an owner in upper case":     {"groups:\n  g:\n    owners: [Geth]\n", `owner "Geth" is not a team name`},
		"a grant as owner":           {"groups:\n  g:\n    owners: [\"geth:all\"]\n", "is not a team name"},
		"an owner twice":             {"groups:\n  g:\n    owners: [geth, geth]\n", `owner "geth" listed twice`},
		"nine owners":                {"groups:\n  g:\n    owners: [a, b, c, d, e, f, g, h, i]\n", "more than 8 owners"},
		"a client in upper case":     {"groups:\n  g:\n    client: Lighthouse\n", `client "Lighthouse" is not a client name`},
		"an unknown impact":          {"groups:\n  g:\n    impact: none\n", `unknown impact "none"`},
		"an unknown part":            {"groups:\n  g:\n    overrides: [env]\n", `unknown part "env"`},
		"a part twice":               {"groups:\n  g:\n    overrides: [image, image]\n", `"image" listed twice`},
		"a negative stop timeout":    {"groups:\n  g:\n    stopTimeout: -1s\n", "stopTimeout is negative"},
		"a long name": {
			"groups:\n  " + strings.Repeat("a", 64) + ": {}\n", "not a group name",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, loadErr := loadPolicy(t, tt.groups)
			require.ErrorContains(t, loadErr, tt.want)
		})
	}

	_, err = loadPolicy(t, "containers: []\n")
	require.Error(t, err, "the name map is a map")
}

func TestPolicyContainersAndDefaultGroup(t *testing.T) {
	p, err := loadPolicy(t, `containers:
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
	}, p.Containers)
	assert.False(t, p.DefaultGroup.Roll, "the default group is not rolled unless asked")
	assert.True(t, p.DefaultGroup.excludes("tmp-1"))
	assert.True(t, p.DefaultGroup.excludes("debug"))
	assert.False(t, p.DefaultGroup.excludes("debugger"))

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
		"an empty exclude pattern":       {"defaultGroup:\n  exclude: ['']\n", "not a name or pattern"},
		"an unknown key in defaultGroup": {"defaultGroup:\n  rolls: true\n", "not found"},
	} {
		t.Run(name, func(t *testing.T) {
			_, loadErr := loadPolicy(t, tt.yaml)
			require.ErrorContains(t, loadErr, tt.want)
		})
	}

	_, err = loadPolicy(t, "packages:\n  blocked: [\"not a package\"]\n")
	require.NoError(t, err, "a bad package name is dropped by host, not rejected here")
}
