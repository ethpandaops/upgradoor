package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

const testSeed = "keygen-test-seed-0123456789abcdef0123456789"

// runKeygen runs the keygen command with the devnet seed in the environment.
func runKeygen(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	t.Setenv(seedEnv, testSeed)
	t.Setenv(controllerSeedEnv, "")

	var out bytes.Buffer

	cmd := newKeygenCmd()
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()

	return out.String(), err
}

// A certificate issued for a host's public key verifies against the signer
// key and certifies that key.
func TestHostCertCertifiesThePublicKey(t *testing.T) {
	key, err := identity.GenerateHostKey()
	require.NoError(t, err)

	pub := identity.EncodePublicKey(identity.PublicKey(key))

	cert, err := runKeygen(t, "", "host-cert", "--network", "devnet-1",
		"--hostname", "lighthouse-geth-1", "--public-key", pub)
	require.NoError(t, err)

	signer, err := runKeygen(t, "", "signer-pubkey", "--network", "devnet-1")
	require.NoError(t, err)

	out, err := runKeygen(t, cert, "verify-cert", "--network", "devnet-1",
		"--signer-public-key", strings.TrimSpace(signer))
	require.NoError(t, err)

	var info identity.CertificateInfo
	require.NoError(t, json.Unmarshal([]byte(out), &info))
	assert.Equal(t, identity.CertificateInfo{
		Hostname: "lighthouse-geth-1", Network: "devnet-1", PublicKey: pub,
	}, info)

	_, err = runKeygen(t, cert, "verify-cert", "--network", "devnet-2",
		"--signer-public-key", strings.TrimSpace(signer))
	require.ErrorIs(t, err, identity.ErrWrongNetwork)
}

func TestHostCertRejectsInput(t *testing.T) {
	key, err := identity.GenerateHostKey()
	require.NoError(t, err)

	pub := identity.EncodePublicKey(identity.PublicKey(key))

	tests := []struct {
		name string
		args []string
	}{
		{"no public key", []string{"--hostname", "a"}},
		{"private key", []string{"--hostname", "a", "--public-key", identity.EncodePrivateKey(key)}},
		{"bad hostname", []string{"--hostname", "a/b", "--public-key", pub}},
		{"seed argument", []string{"--hostname", "a", "--public-key", pub, testSeed}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runKeygen(t, "", append([]string{"host-cert", "--network", "devnet-1"},
				tt.args...)...)
			require.Error(t, err)
			require.Empty(t, out)
		})
	}
}

// A seed file of - is standard input; the seeds still cannot be mixed up.
func TestSeedFromStdin(t *testing.T) {
	const ctrlSeed = "keygen-test-controller-seed-0123456789abcdef"

	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer

		cmd := newKeygenCmd()
		cmd.SetArgs(args)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})

		err := cmd.Execute()

		return out.String(), err
	}

	t.Setenv(seedEnv, testSeed)
	t.Setenv(controllerSeedEnv, ctrlSeed)

	fromEnv, err := run("", "signer-pubkey", "--network", "devnet-1")
	require.NoError(t, err)

	ctrlFromEnv, err := run("", "controller-pubkey", "--network", "devnet-1")
	require.NoError(t, err)

	t.Setenv(seedEnv, "")
	t.Setenv(controllerSeedEnv, "")

	out, err := run(testSeed+"\n", "signer-pubkey", "--network", "devnet-1", "--seed-file", "-")
	require.NoError(t, err)
	assert.Equal(t, fromEnv, out)

	out, err = run(ctrlSeed, "controller-pubkey", "--network", "devnet-1",
		"--controller-seed-file", "-")
	require.NoError(t, err)
	assert.Equal(t, ctrlFromEnv, out)

	_, err = run(ctrlSeed, "controller-pubkey", "--network", "devnet-1", "--seed-file", "-")
	require.ErrorContains(t, err, "is the devnet seed")

	_, err = run("short", "signer-pubkey", "--network", "devnet-1", "--seed-file", "-")
	require.ErrorIs(t, err, identity.ErrSeedTooShort)

	t.Setenv(controllerSeedEnv, testSeed)

	_, err = run(testSeed, "signer-pubkey", "--network", "devnet-1", "--seed-file", "-")
	require.ErrorIs(t, err, errSameSeed)
}
