package overrides

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageAllowed(t *testing.T) {
	patterns := []string{"ethpandaops/geth", "ghcr.io/ethereum/*", "registry.example:5000/team/*-debug"}

	for ref, want := range map[string]bool{
		"ethpandaops/geth:fix":                    true,
		"docker.io/ethpandaops/geth:fix":          true,
		"index.docker.io/ethpandaops/geth":        true,
		"ethpandaops/geth-debug:fix":              false,
		"ethpandaops/besu:fix":                    false,
		"ghcr.io/ethereum/go-ethereum:x":          true,
		"ghcr.io/ethereum/a/b:x":                  false,
		"ghcr.io/other/go-ethereum:x":             false,
		"registry.example:5000/team/geth-debug:1": true,
		"registry.example:5000/team/geth:1":       false,
		"evil.example/ethpandaops/geth:fix":       false,
		"not a reference":                         false,
	} {
		assert.Equal(t, want, imageAllowed(patterns, ref), ref)
	}

	assert.False(t, imageAllowed(nil, "ethpandaops/geth:fix"), "no pattern allows nothing")
	assert.True(t, imageAllowed([]string{"nginx"}, "nginx:1"), "an official image")

	require.NoError(t, validateRepositoryPattern("registry.example:5000/team/*"))
	require.Error(t, validateRepositoryPattern("ethpandaops/geth:master"))
	require.Error(t, validateRepositoryPattern("ethpandaops/[geth"))
	require.Error(t, validateRepositoryPattern(""))
}
