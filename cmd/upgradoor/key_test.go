package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

func runKey(t *testing.T, path string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	cmd := newKeyCmd()
	cmd.SetArgs([]string{"--path", path})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()

	return strings.TrimSpace(out.String()), err
}

// The key is created once, readable by its directory's group, and kept.
func TestKeyIsCreatedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.key")

	first, err := runKey(t, path)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	key, err := identity.DecodePrivateKey(string(raw))
	require.NoError(t, err)
	assert.Equal(t, identity.EncodePublicKey(identity.PublicKey(key)), first)

	second, err := runKey(t, path)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left behind")
}

func TestKeyRefusesABrokenKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.key")
	require.NoError(t, os.WriteFile(path, []byte("nope\n"), 0o600))

	_, err := runKey(t, path)
	require.ErrorIs(t, err, identity.ErrInvalidKey)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "nope\n", string(raw))
}
