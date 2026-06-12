package sori

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HeaInSeo/sori/archiveutil"
)

func loadMetadataJSON(path string) ([]byte, error) {
	// #nosec G304 -- metadata path is supplied by packaging/fetch callers and validated by higher-level flows.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, notFoundError("loadMetadataJSON", fmt.Sprintf("read JSON file %s", path), err)
		}
		return nil, transportError("loadMetadataJSON", fmt.Sprintf("read JSON file %s", path), err)
	}
	var tmp interface{}
	if err := json.Unmarshal(data, &tmp); err != nil {
		return nil, validationError("loadMetadataJSON", fmt.Sprintf("invalid JSON in %s", path), err)
	}
	return data, nil
}

// GenerateVolumeIndex builds a VolumeIndex from the top-level subdirectories of
// rootPath. Only immediate child directories become partitions; nested directories
// are included in their parent partition's layer. Root-level regular files are
// NOT listed as partitions — they are handled as a separate layer during publish.
func GenerateVolumeIndex(rootPath, displayName string) (*VolumeIndex, error) {
	now := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	rootBase := filepath.Base(rootPath)

	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return nil, transportError("GenerateVolumeIndex", fmt.Sprintf("read root dir %s", rootPath), err)
	}

	var parts []Partition
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		parts = append(parts, Partition{
			Name:        d.Name(),
			Path:        rootBase + "/" + d.Name(),
			ManifestRef: "",
			CreatedAt:   now,
			Compression: "",
		})
	}

	return &VolumeIndex{
		VolumeRef:   "",
		DisplayName: displayName,
		CreatedAt:   now,
		Partitions:  parts,
	}, nil
}

func (vi *VolumeIndex) SaveToFile(rootPath string) error {
	outFile := filepath.Join(rootPath, VolumeIndexJson)
	data, err := json.MarshalIndent(vi, "", "  ")
	if err != nil {
		return transportError("VolumeIndex.SaveToFile", "marshal volume index", err)
	}
	if err := writeFileAtomic(outFile, data, 0o644); err != nil {
		return transportError("VolumeIndex.SaveToFile", fmt.Sprintf("write file %s", outFile), err)
	}
	return nil
}

func ValidateVolumeDir(volDir string) ([]byte, error) {
	info, err := os.Stat(volDir)
	if err != nil {
		return nil, notFoundError("ValidateVolumeDir", fmt.Sprintf("volume dir %q does not exist", volDir), err)
	}
	if !info.IsDir() {
		return nil, validationError("ValidateVolumeDir", fmt.Sprintf("volume path %q is not a directory", volDir), nil)
	}

	entries, err := os.ReadDir(volDir)
	if err != nil {
		return nil, transportError("ValidateVolumeDir", fmt.Sprintf("read directory %q", volDir), err)
	}
	visibleCount := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			Log.Warnf("ValidateVolumeDir: hidden entry %q found in %s, skipping", name, volDir)
			continue
		}
		if name == ConfigBlobJson {
			continue
		}
		visibleCount++
	}
	if visibleCount == 0 {
		return nil, validationError("ValidateVolumeDir", fmt.Sprintf("volume directory %q is empty (only hidden files present)", volDir), nil)
	}

	cfgPath := filepath.Join(volDir, ConfigBlobJson)
	raw, err := loadMetadataJSON(cfgPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			Log.Infof("ValidateVolumeDir: %q not found; creating a new empty configblob.json", cfgPath)
			raw = []byte("{}")
			if writeErr := writeFileAtomic(cfgPath, raw, 0o644); writeErr != nil {
				return nil, transportError("ValidateVolumeDir", fmt.Sprintf("create %q", cfgPath), writeErr)
			}
		} else {
			return nil, err
		}
	}
	return raw, nil
}

// readLocalVolumeIndex reads destRoot/volume-index.json and returns the stored
// VolumeIndex. Any error (file absent, malformed JSON) is returned as-is; the
// caller should treat a non-nil error as "no valid local index" and proceed
// with a full fetch.
func readLocalVolumeIndex(destRoot string) (*VolumeIndex, error) {
	// #nosec G304 -- reads the fixed volume-index.json under caller-selected destRoot.
	data, err := os.ReadFile(filepath.Join(destRoot, VolumeIndexJson))
	if err != nil {
		return nil, err
	}
	var vi VolumeIndex
	if err := json.Unmarshal(data, &vi); err != nil {
		return nil, err
	}
	return &vi, nil
}

func writeVolumeIndex(destRoot string, vi *VolumeIndex) error {
	if err := os.MkdirAll(destRoot, 0o750); err != nil {
		return transportError("writeVolumeIndex", fmt.Sprintf("create destination root %s", destRoot), err)
	}

	indexPath := filepath.Join(destRoot, VolumeIndexJson)
	indexBytes, err := json.MarshalIndent(vi, "", "  ")
	if err != nil {
		return transportError("writeVolumeIndex", "marshal VolumeIndex", err)
	}
	if err := writeFileAtomic(indexPath, indexBytes, 0o644); err != nil {
		return transportError("writeVolumeIndex", fmt.Sprintf("write %s", indexPath), err)
	}
	return nil
}

func dirRegularFileSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return transportError("dirRegularFileSize", fmt.Sprintf("walk %s", path), err)
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return transportError("dirRegularFileSize", fmt.Sprintf("stat %s", path), err)
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func validateJSONBytes(data []byte) error {
	var tmp interface{}
	if err := json.Unmarshal(data, &tmp); err != nil {
		return validationError("validateJSONBytes", "invalid JSON payload", err)
	}
	return nil
}

func UntarGzDir(gzipStream io.Reader, dest string) error {
	return archiveutil.UntarGzDir(gzipStream, dest)
}

func TarGzDir(fsDir, prefixPath string) ([]byte, error) {
	return archiveutil.TarGzDir(fsDir, prefixPath)
}
