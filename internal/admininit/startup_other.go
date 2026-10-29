//go:build !linux

package admininit

import "errors"

// readAdminFacts has no reader outside Linux: the admin-tail readers use
// openat with O_NOFOLLOW on directory descriptors. The epoch is reported
// present and unreadable, so every start classifies RO/HOLD (S0).
func readAdminFacts(string) adminFacts {
	return adminFacts{epochPresent: true, epochErr: &TailError{
		Record: EpochName, Kind: TailUnreadable, Err: errors.New("the admin-control reader is implemented for Linux only"),
	}}
}
