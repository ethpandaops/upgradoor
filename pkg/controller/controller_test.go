package controller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

func TestLoadAnswerSigner(t *testing.T) {
	const (
		network        = "testnet-devnet-0"
		controllerSeed = "fedcba9876543210fedcba9876543210"
	)

	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		return path
	}

	publicKey := func(seed string) string {
		key, err := identity.DeriveControllerKey([]byte(seed))
		require.NoError(t, err)

		return identity.EncodePublicKey(identity.PublicKey(key))
	}

	signerKey := func(t *testing.T, cfg *Config) string {
		t.Helper()

		signer, err := loadAnswerSigner(cfg)
		require.NoError(t, err)
		require.NotNil(t, signer)

		return identity.EncodePublicKey(signer.PublicKey())
	}

	// The controller derives the key 'keygen controller-pubkey' gives the
	// agents. A trailing newline of the file is not part of the seed.
	good := write("controller.seed", controllerSeed+"\n")

	assert.Equal(t, publicKey(controllerSeed),
		signerKey(t, seedConfig(network, good)))

	// The key does not depend on the network.
	other := signerKey(t, seedConfig("testnet-devnet-1", good))
	assert.Equal(t, publicKey(controllerSeed), other)

	// Without a seed (the opt-out, which Validate checks) there is no key.
	signer, err := loadAnswerSigner(&Config{Network: NetworkConfig{Name: network}})
	require.NoError(t, err)
	assert.Nil(t, signer)

	_, err = loadAnswerSigner(seedConfig(network, filepath.Join(dir, "missing")))
	require.ErrorIs(t, err, identity.ErrNoSeed)

	// A seed that is too short is refused, and nothing of the file is in
	// the error, which is logged.
	const short = "too-short-to-be-a-seed"

	for _, content := range []string{short, short + "\n", ""} {
		_, err = loadAnswerSigner(seedConfig(network, write("short.seed", content)))
		require.ErrorIs(t, err, identity.ErrSeedTooShort)
		assert.NotContains(t, err.Error(), short)
	}
}

func seedConfig(network, seedFile string) *Config {
	return &Config{Network: NetworkConfig{Name: network}, Agents: AgentsConfig{ControllerSeedFile: seedFile}}
}
