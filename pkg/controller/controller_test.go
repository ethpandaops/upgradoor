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

func TestParseDefaultSettings(t *testing.T) {
	s, err := ParseDefaultSettings(nil)
	require.NoError(t, err)
	assert.Equal(t, fleet.DefaultSettings(), s)

	s, err = ParseDefaultSettings([]string{
		"maxUnavailablePercent=25", "leaseTtl=10m", "policyReboot=auto",
		"readyEpochs=2", "minPeers=5", "paused=true", `policyImage="manual"`,
		"readyPeerShare=0.25", "pauseAfterCanaryImage=true",
	})
	require.NoError(t, err)
	assert.InDelta(t, 25.0, s.MaxUnavailablePercent, 0)
	assert.Equal(t, 10*time.Minute, s.LeaseTTL.Std())
	assert.Equal(t, fleet.PolicyAuto, s.PolicyReboot)
	assert.Equal(t, fleet.PolicyManual, s.PolicyImage)
	assert.Equal(t, uint64(2), s.ReadyEpochs)
	assert.Equal(t, 5, s.MinPeers)
	assert.True(t, s.Paused)
	assert.InDelta(t, 0.25, s.ReadyPeerShare, 0)
	assert.True(t, s.PauseAfterCanaryImage)
	assert.False(t, s.PauseAfterCanaryReboot)

	for _, bad := range []string{
		"nokey", "=1", "unknown=1", "leaseTtl=30", "maxUnavailablePercent=200",
		"policyImage=sometimes", "readyEpochs=many", "minPeers=0",
		"readyPeerShare=2", "readyPeerShare=half", "pauseAfterCanaryImage=sometimes",
	} {
		_, err := ParseDefaultSettings([]string{bad})
		require.Error(t, err, bad)
	}
}

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

	publicKey := func(seed string, net string) string {
		key, err := identity.DeriveControllerKey([]byte(seed), net)
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

	// The controller derives the key of its own network, the
	// one 'keygen controller-pubkey' gives the agents. A trailing newline of
	// the file is not part of the seed.
	good := write("controller.seed", controllerSeed+"\n")

	assert.Equal(t, publicKey(controllerSeed, network),
		signerKey(t, &Config{Network: network, ControllerSeedFile: good}))

	// Every devnet of a group has a key of its own from the one seed.
	other := signerKey(t, &Config{Network: "testnet-devnet-1", ControllerSeedFile: good})
	assert.Equal(t, publicKey(controllerSeed, "testnet-devnet-1"), other)
	assert.NotEqual(t, publicKey(controllerSeed, network), other)

	// The opt-out is the only way to run without a seed.
	signer, err := loadAnswerSigner(&Config{Network: network, AllowUnsignedAgentAnswers: true})
	require.NoError(t, err)
	assert.Nil(t, signer)

	_, err = loadAnswerSigner(&Config{Network: network})
	require.ErrorContains(t, err, "--controller-seed-file is required")

	_, err = loadAnswerSigner(&Config{
		Network: network, ControllerSeedFile: good, AllowUnsignedAgentAnswers: true,
	})
	require.ErrorContains(t, err, "exclude each other")

	_, err = loadAnswerSigner(&Config{
		Network: network, ControllerSeedFile: filepath.Join(dir, "missing"),
	})
	require.ErrorIs(t, err, identity.ErrNoSeed)

	// A seed that is too short is refused, and nothing of the file is in
	// the error, which is logged.
	const short = "too-short-to-be-a-seed"

	for _, content := range []string{short, short + "\n", ""} {
		_, err = loadAnswerSigner(&Config{
			Network: network, ControllerSeedFile: write("short.seed", content),
		})
		require.ErrorIs(t, err, identity.ErrSeedTooShort)
		assert.NotContains(t, err.Error(), short)
	}
}
