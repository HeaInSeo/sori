//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package admininit

import "syscall"

// openNoFollow makes bind's open of an existing database fail on a symlink at
// the final name and return at once on a FIFO, which bind then refuses.
const openNoFollow = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
