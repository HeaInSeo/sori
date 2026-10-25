//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package adminlock

import (
	"errors"
	"os"
	"syscall"
)

const supported = true

// tryLock takes a shared or exclusive flock on f without waiting. It reports false
// when another open file holds a conflicting lock.
func tryLock(f *os.File, exclusive bool) (bool, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		// #nosec G115 -- a file descriptor always fits in int.
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}
