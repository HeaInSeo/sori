//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package admininit

import (
	"io/fs"
	"syscall"
)

// openNoFollow makes bind's open of an existing database fail on a symlink at
// the final name and return at once on a FIFO, which bind then refuses.
const openNoFollow = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// linkCount returns the number of hard links to the file fi describes, and
// false if the platform does not report it.
func linkCount(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true //nolint:unconvert // Nlink is uint16 on darwin and uint32 on some BSDs.
}
