// Package atomicfile replaces files so that a crash at any point leaves
// either the old or the new content, never a mix.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// TempPattern is appended to a file's path for the temp file a replacement
// writes first. What matches it beside a file is left over from a write that
// was cut short.
const TempPattern = ".tmp-*"

// Write replaces path with data so that a crash at any point
// leaves either the old or the new content, never a mix: temp file in the
// same directory, fsync, rename, fsync of the directory.
func Write(path string, data []byte, perm os.FileMode) error {
	if err := Replace(path, data, perm); err != nil {
		return err
	}

	return SyncDir(filepath.Dir(path))
}

// Replace is Write up to and including the rename. After an
// error path still has its previous content; after success it has data,
// durably once its directory is fsynced.
func Replace(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+TempPattern)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()

		return fmt.Errorf("write temp file: %w", err)
	}

	if err := tmp.Chmod(perm); err != nil {
		cleanup()

		return fmt.Errorf("chmod temp file: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		cleanup()

		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}

// SyncDir fsyncs a directory so that a rename or removal in it is durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir: %w", err)
	}

	if err := d.Sync(); err != nil {
		_ = d.Close()

		return fmt.Errorf("sync dir: %w", err)
	}

	if err := d.Close(); err != nil {
		return fmt.Errorf("close dir: %w", err)
	}

	return nil
}
