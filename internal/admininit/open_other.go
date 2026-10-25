//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package admininit

import "io/fs"

// openNoFollow is zero where the flags do not exist. bind still refuses a held
// file that is not a regular file or not the one the preflight saw.
const openNoFollow = 0

// linkCount reports false: os.Lstat does not return a link count here, so a
// hard-linked database is not detected on this platform.
func linkCount(fs.FileInfo) (uint64, bool) {
	return 0, false
}
