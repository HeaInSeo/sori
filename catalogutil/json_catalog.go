package catalogutil

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path atomically using a tmpfile+rename pattern.
// Sync is called before Close so data reaches stable storage before rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
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

func LoadOrInit[T any](rootDir, fileName string, zero T) (*T, error) {
	path := filepath.Join(rootDir, fileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &zero, nil
		}
		return nil, transportError("LoadOrInit", "read catalog "+path, err)
	}

	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, validationError("LoadOrInit", "unmarshal catalog "+path, err)
	}
	return &out, nil
}

func Save(rootDir, fileName string, value any) error {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return transportError("Save", "create catalog dir "+rootDir, err)
	}
	path := filepath.Join(rootDir, fileName)
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return transportError("Save", "marshal catalog "+path, err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return transportError("Save", "write catalog "+path, err)
	}
	return nil
}
