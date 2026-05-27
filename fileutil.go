package sori

import (
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path atomically using a tmpfile+rename pattern.
// It creates a temporary file in the same directory as path, writes the data,
// syncs to storage, sets the file permissions, and renames it to the final
// path. On failure it removes the temporary file.
//
// The Sync call ensures data reaches stable storage before rename, preventing
// a power-failure from producing a zero-length file at the target path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}

	return nil
}
