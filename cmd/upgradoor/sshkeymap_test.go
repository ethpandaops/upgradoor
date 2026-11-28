package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/logins"
)

func TestWriteKeyMap(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the file is chowned to a group of the system")
	}

	path := filepath.Join(t.TempDir(), "ssh-keys.json")
	m := logins.KeyMap{"SHA256:x": {Identity: "github:pk910", Source: logins.SourceGitHub, User: "devops"}}

	require.NoError(t, writeKeyMap(path, "upgradoor", m))
	require.NoError(t, writeKeyMap(path, "upgradoor", m), "replaced in place")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, keyMapMode, info.Mode().Perm())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	got, err := logins.ParseKeyMap(raw)
	require.NoError(t, err)
	assert.Equal(t, m, got)

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left")
}

func TestSSHKeyMapCmd(t *testing.T) {
	cmd := newSSHKeyMapCmd()
	cmd.SetArgs([]string{"--out", filepath.Join(t.TempDir(), "k.json")})
	require.ErrorContains(t, cmd.Execute(), "--user")

	cmd = newSSHKeyMapCmd()
	cmd.SetArgs([]string{"--user", "no-such-user-here", "--out", filepath.Join(t.TempDir(), "k.json")})
	require.ErrorContains(t, cmd.Execute(), "no-such-user-here")
}
