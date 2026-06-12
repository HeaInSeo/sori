package sori

import (
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path using a temp-file + rename pattern.
// Sync before Close flushes data to the OS page cache; this prevents a
// zero-byte file at the target path after an abrupt process exit but does
// not guarantee full crash consistency (no parent-directory fsync).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		removeAtomicTempFile(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		removeAtomicTempFile(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		removeAtomicTempFile(tmpName)
		return err
	}

	if err := os.Chmod(tmpName, perm); err != nil {
		removeAtomicTempFile(tmpName)
		return err
	}

	if err := os.Rename(tmpName, path); err != nil {
		removeAtomicTempFile(tmpName)
		return err
	}

	return nil
}

func removeAtomicTempFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		// Best-effort cleanup only; preserve the original write/rename error.
		return
	}
}
