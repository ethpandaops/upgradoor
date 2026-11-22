package local

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
)

func writeManaged(t *testing.T, path, container string, d plan.Deployed) bool {
	t.Helper()

	var changed bool

	require.NoError(t, WriteManaged(path, container, d, func(c bool) error {
		changed = c

		return nil
	}))

	return changed
}

func TestWriteManaged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed.yaml")

	beacon := plan.Deployed{Image: "sigp/lighthouse:latest", Command: []string{"lighthouse", "beacon_node"}, HasCommand: true}
	assert.True(t, writeManaged(t, path, "beacon", beacon))
	assert.True(t, writeManaged(t, path, "execution", plan.Deployed{Image: "ethereum/client-go:stable"}))
	assert.True(t, writeManaged(t, path, "dora", plan.Deployed{Image: "ethpandaops/dora:1", HasCommand: true}))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, `# written by upgradoor deploy, do not edit
containers:
  beacon:
    image: sigp/lighthouse:latest
    command:
      - lighthouse
      - beacon_node
  dora:
    image: ethpandaops/dora:1
    command: []
  execution:
    image: ethereum/client-go:stable
`, string(raw))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(managedMode), info.Mode().Perm())

	dir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)

	_, fileGid, _ := ownerOf(info)
	_, dirGid, _ := ownerOf(dir)
	assert.Equal(t, dirGid, fileGid, "the directory's group reads it")

	got, err := ReadManaged(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]plan.Deployed{
		"beacon":    beacon,
		"execution": {Image: "ethereum/client-go:stable"},
		"dora":      {Image: "ethpandaops/dora:1", Command: []string{}, HasCommand: true},
	}, got)

	// The same entry again: not written.
	assert.False(t, writeManaged(t, path, "beacon", beacon))

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(info, after))

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 2, "managed.yaml and its lock, no temp file")
}

// Deploys running at once are serialised: none loses another's entry, and
// then runs under the lock.
func TestWriteManagedIsLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed.yaml")

	var wg sync.WaitGroup

	for i := range 8 {
		wg.Go(func() {
			err := WriteManaged(path, fmt.Sprintf("c%d", i), plan.Deployed{Image: "a/b:1"}, func(bool) error {
				lock, err := os.Open(path + lockSuffix)
				if err != nil {
					return err
				}

				defer func() { _ = lock.Close() }()

				if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
					return fmt.Errorf("the lock is free during then")
				}

				return nil
			})
			assert.NoError(t, err)
		})
	}

	wg.Wait()

	got, err := ReadManaged(path)
	require.NoError(t, err)
	assert.Len(t, got, 8)
}

func TestWriteManagedRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed.yaml")
	never := func(bool) error {
		t.Fatal("then called")

		return nil
	}

	require.ErrorContains(t, WriteManaged(path, "../x", plan.Deployed{Image: "a/b:1"}, never), "not a valid container name")
	require.ErrorContains(t, WriteManaged(path, "geth", plan.Deployed{Image: "A b"}, never), "image")
	require.ErrorContains(t, WriteManaged(path, "geth",
		plan.Deployed{Image: "a/b:1", Command: []string{"a\nb"}, HasCommand: true}, never), "command")
	assert.NoFileExists(t, path)

	require.NoError(t, os.WriteFile(path, []byte("containers: [broken\n"), 0o600))
	require.ErrorContains(t, WriteManaged(path, "geth", plan.Deployed{Image: "a/b:1"}, never), "delete it")

	if os.Geteuid() == 0 {
		return
	}

	locked := filepath.Join(dir, "ro")
	require.NoError(t, os.Mkdir(locked, 0o500))
	require.ErrorIs(t, WriteManaged(filepath.Join(locked, "managed.yaml"), "geth", plan.Deployed{Image: "a/b:1"}, never),
		fs.ErrPermission)
}

func TestReadManaged(t *testing.T) {
	dir := t.TempDir()

	got, err := ReadManaged(filepath.Join(dir, "missing.yaml"))
	require.NoError(t, err)
	assert.Empty(t, got, "a missing file deploys nothing")

	for name, content := range map[string]string{
		"unknown key": "containers:\n  geth:\n    image: a/b:1\n    flags: []\n",
		"no image":    "containers:\n  geth:\n    command: []\n",
		"bad name":    "containers:\n  \"../x\":\n    image: a/b:1\n",
	} {
		path := filepath.Join(dir, "m.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		_, readErr := ReadManaged(path)
		require.Error(t, readErr, name)
	}

	link := filepath.Join(dir, "link.yaml")
	require.NoError(t, os.Symlink(filepath.Join(dir, "m.yaml"), link))

	_, err = ReadManaged(link)
	require.EqualError(t, err, "managed.yaml is a symbolic link")
}
