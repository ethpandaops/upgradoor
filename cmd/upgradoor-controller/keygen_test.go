package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestHostCertsMatchesSingleCommands(t *testing.T) {
	in := `[
		{"hostname": "lighthouse-geth-1", "roles": ["validator", "bootnode"]},
		{"hostname": "teku-besu-2"},
		{"hostname": "bootnode-1", "roles": []}
	]`

	out, err := runKeygen(t, in, "host-certs", "--network", "devnet-1")
	require.NoError(t, err)

	var got map[string]hostFiles
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got, 3)

	single := []struct {
		hostname string
		flags    []string
	}{
		{"lighthouse-geth-1", []string{"--role", "validator", "--role", "bootnode"}},
		{"teku-besu-2", nil},
		{"bootnode-1", nil},
	}

	for _, h := range single {
		base := []string{"--network", "devnet-1", "--hostname", h.hostname}

		key, err := runKeygen(t, "", append(append([]string{"host-key"}, base...),
			flagsWithoutRoles(h.flags)...)...)
		require.NoError(t, err)

		cert, err := runKeygen(t, "", append(append([]string{"host-cert"}, base...), h.flags...)...)
		require.NoError(t, err)

		require.Equal(t, hostFiles{Key: key, Cert: cert}, got[h.hostname], h.hostname)
	}
}

// flagsWithoutRoles drops the --role pairs host-key does not take.
func flagsWithoutRoles(flags []string) []string {
	out := make([]string, 0, len(flags))

	for i := 0; i < len(flags); i++ {
		if flags[i] == "--role" {
			i++

			continue
		}

		out = append(out, flags[i])
	}

	return out
}

func TestHostCertsRejectsInput(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"duplicate", `[{"hostname": "a"}, {"hostname": "b"}, {"hostname": "a"}]`,
			"host 2 (a): duplicate of host 0"},
		{"bad hostname", `[{"hostname": "a"}, {"hostname": "a/b"}]`, "host 1: hostname"},
		{"empty hostname", `[{"roles": []}]`, "host 0: hostname"},
		{"bad role", `[{"hostname": "a", "roles": ["x y"]}]`, "host 0 (a): role"},
		{"unknown field", `[{"hostname": "a", "seed": "x"}]`, "host 0:"},
		{"not an array", `{"hostname": "a"}`, "not a JSON array"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runKeygen(t, tt.in, "host-certs", "--network", "devnet-1")
			require.ErrorContains(t, err, tt.want)
			require.Empty(t, out)
		})
	}
}

func TestHostCertsRefusesSeedArgument(t *testing.T) {
	_, err := runKeygen(t, "[]", "host-certs", "--network", "devnet-1", testSeed)
	require.Error(t, err)

	_, err = runKeygen(t, "[]", "host-certs", "--network", "devnet-1", "--seed", testSeed)
	require.Error(t, err)
}
