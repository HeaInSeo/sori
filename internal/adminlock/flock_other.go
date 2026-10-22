//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package adminlock

import "os"

const supported = false

func tryLock(*os.File, bool) (bool, error) {
	return false, ErrUnsupported
}
