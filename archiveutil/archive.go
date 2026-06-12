package archiveutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TarGzDirTo writes a gzip-compressed tar of fsDir into w.
// prefixPath is used as the tar name prefix (same semantics as TarGzDir).
func TarGzDirTo(w io.Writer, fsDir, prefixPath string) error {
	var entries []string
	if err := filepath.WalkDir(fsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return transportError("TarGzDirTo", "walk source directory", err)
		}
		entries = append(entries, path)
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(entries)

	gw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return transportError("TarGzDirTo", "create gzip writer", err)
	}
	gw.ModTime = time.Unix(0, 0)
	gw.OS = 0

	tw := tar.NewWriter(gw)
	for _, path := range entries {
		info, err := os.Lstat(path)
		if err != nil {
			return transportError("TarGzDirTo", "stat source path "+path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return validationError("TarGzDirTo", "symlinks are not supported: "+path, nil)
		}
		rel, err := filepath.Rel(fsDir, path)
		if err != nil {
			return transportError("TarGzDirTo", "resolve relative path "+path, err)
		}

		var tarName string
		if rel == "." {
			tarName = prefixPath
		} else {
			tarName = filepath.ToSlash(filepath.Join(prefixPath, rel))
		}
		if tarName == "" {
			// Root directory with no prefix produces an empty name, which
			// UntarGzDir correctly rejects. Skip it — dest is created by
			// the caller.
			continue
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return transportError("TarGzDirTo", "build tar header for "+path, err)
		}
		hdr.Name = tarName
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""
		hdr.ModTime = time.Unix(0, 0)

		if err := tw.WriteHeader(hdr); err != nil {
			return transportError("TarGzDirTo", "write tar header for "+path, err)
		}
		if info.Mode().IsRegular() {
			// #nosec G304 -- path comes from filepath.WalkDir over caller-provided fsDir and was Lstat-checked above.
			f, err := os.Open(path)
			if err != nil {
				return transportError("TarGzDirTo", "open source file "+path, err)
			}
			if _, err := io.Copy(tw, f); err != nil {
				cErr := f.Close()
				if cErr != nil {
					return transportError("TarGzDirTo", "copy source file "+path, errors.Join(err, cErr))
				}
				return transportError("TarGzDirTo", "copy source file "+path, err)
			}
			if err := f.Close(); err != nil {
				return transportError("TarGzDirTo", "close source file "+path, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		return transportError("TarGzDirTo", "close tar writer", err)
	}
	if err := gw.Close(); err != nil {
		return transportError("TarGzDirTo", "close gzip writer", err)
	}
	return nil
}

func TarGzDir(fsDir, prefixPath string) ([]byte, error) {
	buf := &bytes.Buffer{}
	if err := TarGzDirTo(buf, fsDir, prefixPath); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TarGzDirFilesTo writes a gzip-compressed tar of only the regular files
// immediately inside fsDir into w. Returns (hasFiles bool, err error).
// hasFiles is false if there are no files to write (nothing was written to w).
func TarGzDirFilesTo(w io.Writer, fsDir, prefixPath string, skipNames map[string]struct{}) (bool, error) {
	entries, err := os.ReadDir(fsDir)
	if err != nil {
		return false, transportError("TarGzDirFilesTo", "read directory "+fsDir, err)
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
		return false, nil
	}

	sort.Strings(filePaths)

	gw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return false, transportError("TarGzDirFilesTo", "create gzip writer", err)
	}
	gw.ModTime = time.Unix(0, 0)
	gw.OS = 0

	tw := tar.NewWriter(gw)
	for _, path := range filePaths {
		info, err := os.Lstat(path)
		if err != nil {
			return false, transportError("TarGzDirFilesTo", "stat "+path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, validationError("TarGzDirFilesTo", "symlinks are not supported: "+path, nil)
		}
		tarName := filepath.ToSlash(filepath.Join(prefixPath, filepath.Base(path)))
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return false, transportError("TarGzDirFilesTo", "build tar header for "+path, err)
		}
		hdr.Name = tarName
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""
		hdr.ModTime = time.Unix(0, 0)
		if err := tw.WriteHeader(hdr); err != nil {
			return false, transportError("TarGzDirFilesTo", "write tar header for "+path, err)
		}
		if info.Mode().IsRegular() {
			// #nosec G304 -- path is a caller-provided source file and was Lstat-checked above.
			f, err := os.Open(path)
			if err != nil {
				return false, transportError("TarGzDirFilesTo", "open "+path, err)
			}
			if _, err := io.Copy(tw, f); err != nil {
				cErr := f.Close()
				if cErr != nil {
					return false, transportError("TarGzDirFilesTo", "copy "+path, errors.Join(err, cErr))
				}
				return false, transportError("TarGzDirFilesTo", "copy "+path, err)
			}
			if err := f.Close(); err != nil {
				return false, transportError("TarGzDirFilesTo", "close "+path, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return false, transportError("TarGzDirFilesTo", "close tar writer", err)
	}
	if err := gw.Close(); err != nil {
		return false, transportError("TarGzDirFilesTo", "close gzip writer", err)
	}
	return true, nil
}

// TarGzDirFiles creates a gzip-compressed tar of only the regular files and
// symlinks immediately inside fsDir (no subdirectory recursion). Files whose
// base names match skipNames are excluded. Returns nil if there are no files
// to include.
func TarGzDirFiles(fsDir, prefixPath string, skipNames map[string]struct{}) ([]byte, error) {
	buf := &bytes.Buffer{}
	hasFiles, err := TarGzDirFilesTo(buf, fsDir, prefixPath, skipNames)
	if err != nil {
		return nil, err
	}
	if !hasFiles {
		return nil, nil
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
		if err := extractTarEntry("UntarGzDir", tr, hdr, target); err != nil {
			return err
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

// UntarGzDirUnderPrefix extracts a gzip-compressed tar into dest.
// Every tar entry's path must begin with requiredPrefix+"/" or be exactly
// requiredPrefix. Entries outside that prefix return ErrIntegrity.
func UntarGzDirUnderPrefix(gzipStream io.Reader, dest, requiredPrefix string) error {
	return untarGzDirFiltered(gzipStream, dest, requiredPrefix, false)
}

// UntarGzDirRootFilesOnly extracts a gzip-compressed tar into dest.
// Only the directory entry for requiredPrefix itself, and regular files
// directly under requiredPrefix (no subdirectories), are allowed.
// Entries that go deeper return ErrIntegrity.
func UntarGzDirRootFilesOnly(gzipStream io.Reader, dest, requiredPrefix string) error {
	return untarGzDirFiltered(gzipStream, dest, requiredPrefix, true)
}

func untarGzDirFiltered(gzipStream io.Reader, dest, requiredPrefix string, rootFilesOnly bool) error {
	destRoot, err := filepath.Abs(dest)
	if err != nil {
		return transportError("UntarGzDirFiltered", "resolve destination "+dest, err)
	}
	gz, err := gzip.NewReader(gzipStream)
	if err != nil {
		return integrityError("UntarGzDirFiltered", "create gzip reader", err)
	}
	defer gz.Close()

	prefix := filepath.ToSlash(filepath.Clean(requiredPrefix))

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return integrityError("UntarGzDirFiltered", "read tar entry", err)
		}

		name := filepath.ToSlash(filepath.Clean(hdr.Name))

		// Reject entries outside the required prefix.
		if name != prefix && !strings.HasPrefix(name, prefix+"/") {
			return integrityError("UntarGzDirFiltered",
				"tar entry "+hdr.Name+" is outside required prefix "+requiredPrefix, nil)
		}

		// The prefix entry itself must be a directory (partition root or volume root).
		if name == prefix && hdr.Typeflag != tar.TypeDir {
			return integrityError("UntarGzDirFiltered",
				"prefix entry must be a directory, got type "+fmt.Sprintf("%d", hdr.Typeflag)+" for "+hdr.Name, nil)
		}

		// For root-files layers: only regular files directly under prefix are allowed.
		if rootFilesOnly && name != prefix {
			suffix := strings.TrimPrefix(name, prefix+"/")
			if strings.Contains(suffix, "/") {
				return integrityError("UntarGzDirFiltered",
					"tar entry "+hdr.Name+" is deeper than one level under prefix "+requiredPrefix, nil)
			}
			if hdr.Typeflag != tar.TypeReg {
				return integrityError("UntarGzDirFiltered",
					"root-files layer entry must be a regular file: "+hdr.Name, nil)
			}
		}

		target, err := SecureJoinArchivePath(destRoot, hdr.Name)
		if err != nil {
			return err
		}
		if err := extractTarEntry("UntarGzDirFiltered", tr, hdr, target); err != nil {
			return err
		}
	}
	return nil
}

// extractTarEntry writes a single tar entry to target on disk.
// Only directory and regular file entries are accepted; symlinks and all other
// types return ErrIntegrity so that package-time and extract-time policies are
// consistent (sori never generates symlinks or special files in artifacts).
func extractTarEntry(caller string, tr *tar.Reader, hdr *tar.Header, target string) error {
	mode := hdr.FileInfo().Mode()
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, mode.Perm()); err != nil {
			return transportError(caller, "mkdir "+target, err)
		}
	case tar.TypeReg:
		parentDir := filepath.Dir(target)
		if err := os.MkdirAll(parentDir, 0o750); err != nil {
			return transportError(caller, "mkdir parent "+parentDir, err)
		}
		// #nosec G304 -- target is derived from SecureJoinArchivePath after rejecting absolute, traversal, and symlink entries.
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
		if err != nil {
			return transportError(caller, "open file "+target, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return transportError(caller, "copy file "+target, err)
		}
		if err := f.Close(); err != nil {
			return transportError(caller, "close file "+target, err)
		}
	case tar.TypeSymlink:
		return integrityError(caller, "symlinks are not supported in archive entries: "+hdr.Name, nil)
	default:
		return integrityError(caller, fmt.Sprintf("unsupported tar entry type %d for %s", hdr.Typeflag, hdr.Name), nil)
	}
	return nil
}
