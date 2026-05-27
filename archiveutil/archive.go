package archiveutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func TarGzDir(fsDir, prefixPath string) ([]byte, error) {
	var entries []string
	if err := filepath.WalkDir(fsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return transportError("TarGzDir", "walk source directory", err)
		}
		entries = append(entries, path)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(entries)

	buf := &bytes.Buffer{}
	gw, err := gzip.NewWriterLevel(buf, gzip.BestCompression)
	if err != nil {
		return nil, transportError("TarGzDir", "create gzip writer", err)
	}
	gw.ModTime = time.Unix(0, 0)
	gw.OS = 0

	tw := tar.NewWriter(gw)
	for _, path := range entries {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, transportError("TarGzDir", "stat source path "+path, err)
		}
		rel, err := filepath.Rel(fsDir, path)
		if err != nil {
			return nil, transportError("TarGzDir", "resolve relative path "+path, err)
		}

		var tarName string
		if rel == "." {
			tarName = prefixPath
		} else {
			tarName = filepath.ToSlash(filepath.Join(prefixPath, rel))
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil, transportError("TarGzDir", "build tar header for "+path, err)
		}
		hdr.Name = tarName
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""
		hdr.ModTime = time.Unix(0, 0)

		if err := tw.WriteHeader(hdr); err != nil {
			return nil, transportError("TarGzDir", "write tar header for "+path, err)
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return nil, transportError("TarGzDir", "open source file "+path, err)
			}
			if _, err := io.Copy(tw, f); err != nil {
				cErr := f.Close()
				if cErr != nil {
					return nil, transportError("TarGzDir", "copy source file "+path, errors.Join(err, cErr))
				}
				return nil, transportError("TarGzDir", "copy source file "+path, err)
			}
			if err := f.Close(); err != nil {
				return nil, transportError("TarGzDir", "close source file "+path, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		return nil, transportError("TarGzDir", "close tar writer", err)
	}
	if err := gw.Close(); err != nil {
		return nil, transportError("TarGzDir", "close gzip writer", err)
	}
	return buf.Bytes(), nil
}

// TarGzDirFiles creates a gzip-compressed tar of only the regular files and
// symlinks immediately inside fsDir (no subdirectory recursion). Files whose
// base names match skipNames are excluded. Returns nil if there are no files
// to include.
func TarGzDirFiles(fsDir, prefixPath string, skipNames map[string]struct{}) ([]byte, error) {
	entries, err := os.ReadDir(fsDir)
	if err != nil {
		return nil, transportError("TarGzDirFiles", "read directory "+fsDir, err)
	}

	var filePaths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, skip := skipNames[e.Name()]; skip {
			continue
		}
		filePaths = append(filePaths, filepath.Join(fsDir, e.Name()))
	}
	if len(filePaths) == 0 {
		return nil, nil
	}

	sort.Strings(filePaths)

	buf := &bytes.Buffer{}
	gw, err := gzip.NewWriterLevel(buf, gzip.BestCompression)
	if err != nil {
		return nil, transportError("TarGzDirFiles", "create gzip writer", err)
	}
	gw.ModTime = time.Unix(0, 0)
	gw.OS = 0

	tw := tar.NewWriter(gw)
	for _, path := range filePaths {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, transportError("TarGzDirFiles", "stat "+path, err)
		}
		tarName := filepath.ToSlash(filepath.Join(prefixPath, filepath.Base(path)))
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil, transportError("TarGzDirFiles", "build tar header for "+path, err)
		}
		hdr.Name = tarName
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""
		hdr.ModTime = time.Unix(0, 0)
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, transportError("TarGzDirFiles", "write tar header for "+path, err)
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return nil, transportError("TarGzDirFiles", "open "+path, err)
			}
			if _, err := io.Copy(tw, f); err != nil {
				cErr := f.Close()
				if cErr != nil {
					return nil, transportError("TarGzDirFiles", "copy "+path, errors.Join(err, cErr))
				}
				return nil, transportError("TarGzDirFiles", "copy "+path, err)
			}
			if err := f.Close(); err != nil {
				return nil, transportError("TarGzDirFiles", "close "+path, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, transportError("TarGzDirFiles", "close tar writer", err)
	}
	if err := gw.Close(); err != nil {
		return nil, transportError("TarGzDirFiles", "close gzip writer", err)
	}
	return buf.Bytes(), nil
}

func UntarGzDir(gzipStream io.Reader, dest string) error {
	destRoot, err := filepath.Abs(dest)
	if err != nil {
		return transportError("UntarGzDir", "resolve destination "+dest, err)
	}
	gz, err := gzip.NewReader(gzipStream)
	if err != nil {
		return integrityError("UntarGzDir", "create gzip reader", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return integrityError("UntarGzDir", "read tar entry", err)
		}

		target, err := SecureJoinArchivePath(destRoot, hdr.Name)
		if err != nil {
			return err
		}
		mode := hdr.FileInfo().Mode()

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode.Perm()); err != nil {
				return transportError("UntarGzDir", "mkdir "+target, err)
			}
		case tar.TypeReg:
			parentDir := filepath.Dir(target)
			if err := os.MkdirAll(parentDir, 0o755); err != nil {
				return transportError("UntarGzDir", "mkdir parent "+parentDir, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
			if err != nil {
				return transportError("UntarGzDir", "open file "+target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return transportError("UntarGzDir", "copy file "+target, err)
			}
			if err := f.Close(); err != nil {
				return transportError("UntarGzDir", "close file "+target, err)
			}
		case tar.TypeSymlink:
			if !filepath.IsLocal(hdr.Linkname) {
				return validationError("UntarGzDir", "symlink target escapes destination", nil)
			}
			parentDir := filepath.Dir(target)
			if err := os.MkdirAll(parentDir, 0o755); err != nil {
				return transportError("UntarGzDir", "mkdir parent for symlink "+parentDir, err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return transportError("UntarGzDir", "create symlink "+target, err)
			}
		default:
			continue
		}
	}
	return nil
}

func SecureJoinArchivePath(destRoot, entryName string) (string, error) {
	entry := filepath.Clean(entryName)
	if entry == "." || entry == string(filepath.Separator) || entry == "" {
		return "", validationError("SecureJoinArchivePath", "invalid archive entry "+entryName, nil)
	}
	if filepath.IsAbs(entry) {
		return "", validationError("SecureJoinArchivePath", "archive entry must be relative: "+entryName, nil)
	}
	target := filepath.Join(destRoot, entry)
	rel, err := filepath.Rel(destRoot, target)
	if err != nil {
		return "", transportError("SecureJoinArchivePath", "resolve archive entry "+entryName, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", validationError("SecureJoinArchivePath", "archive entry escapes destination: "+entryName, nil)
	}
	return target, nil
}
