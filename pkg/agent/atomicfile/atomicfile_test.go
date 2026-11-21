package atomicfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/atomicfile"
)

func TestWrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")

	require.NoError(t, atomicfile.Write(file, []byte("one\n"), 0o640))
	require.NoError(t, atomicfile.Write(file, []byte("two\n"), 0o600))

	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, "two\n", string(raw))

	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	left, err := filepath.Glob(file + atomicfile.TempPattern)
	require.NoError(t, err)
	assert.Empty(t, left, "no temp file is left behind")

	require.Error(t, atomicfile.Write(filepath.Join(file, "below-a-file"), nil, 0o600))
}
