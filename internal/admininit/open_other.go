//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package admininit

// openNoFollow is zero where the flags do not exist. bind still refuses a held
// file that is not a regular file or not the one the preflight saw.
const openNoFollow = 0
