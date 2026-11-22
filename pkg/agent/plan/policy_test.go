package plan

import (
	"bytes"
	"errors"
	"io"
	"testing"

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

func TestPolicy(t *testing.T) {
	p, err := loadPolicy(t, "verbs: [roll, reboot]\n")
	require.NoError(t, err)

	assert.True(t, p.Allows(protocol.VerbRoll))
	assert.False(t, p.Allows(protocol.VerbPackages))
	assert.Equal(t, DefaultIgnore, p.DefaultPolicy, "unlabelled containers are ignored unless asked")
	assert.False(t, p.FallbackEnabled())

	p, err = loadPolicy(t, `defaultPolicy: roll
kindPolicies:
  el: {roll: true}
  snooper-el: {roll: false}
`)
	require.NoError(t, err)
	assert.True(t, p.rollsUnlabelled())
	assert.True(t, p.fallbackCovers(&protocol.Container{Kind: protocol.KindEL}))
	assert.False(t, p.fallbackCovers(&protocol.Container{Kind: "snooper-el"}))
	assert.False(t, p.fallbackCovers(&protocol.Container{Kind: protocol.KindCL}), "an unlisted kind")
	assert.True(t, p.fallbackCovers(&protocol.Container{}), "unlabelled: defaultPolicy")

	for name, tt := range map[string]struct {
		yaml string
		want string
	}{
		"an unknown verb":           {"verbs: [exec]\n", `unknown verb "exec"`},
		"an unknown default policy": {"defaultPolicy: follow\n", `unknown policy "follow"`},
		"a kind in upper case":      {"kindPolicies:\n  EL: {roll: true}\n", `"EL" is not a kind`},
		"the kind none":             {"kindPolicies:\n  none: {roll: true}\n", `"none" is not a kind`},
		"an unknown key":            {"kindPolicies:\n  el: {rolls: true}\n", "not found"},
	} {
		t.Run(name, func(t *testing.T) {
			_, loadErr := loadPolicy(t, tt.yaml)
			require.ErrorContains(t, loadErr, tt.want)
		})
	}

	_, err = loadPolicy(t, "packages:\n  blocked: [\"not a package\"]\n")
	require.NoError(t, err, "a bad package name is dropped by host, not rejected here")
}
