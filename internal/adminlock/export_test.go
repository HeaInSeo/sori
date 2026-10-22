package adminlock

import (
	"context"
	"time"
)

// SetIntervals shortens the retry and WAITING_ADMIN_LOCK log intervals and returns
// a function restoring them.
func SetIntervals(poll, log time.Duration) (restore func()) {
	prevPoll, prevLog := pollInterval, waitLogInterval
	pollInterval, waitLogInterval = poll, log
	return func() { pollInterval, waitLogInterval = prevPoll, prevLog }
}

// SetBeforeLockCreate sets the hook that runs between finding no lock file name
// and creating it, and returns a function restoring the old hook.
func SetBeforeLockCreate(fn func(name string)) (restore func()) {
	prev := beforeLockCreate
	beforeLockCreate = fn
	return func() { beforeLockCreate = prev }
}

// SetBeforeLockPublish sets the hook that runs once a new lock file has its mode
// under its temporary name and before it gets the lock file's name, and returns
// a function restoring the old hook.
func SetBeforeLockPublish(fn func(name, tmp string)) (restore func()) {
	prev := beforeLockPublish
	beforeLockPublish = fn
	return func() { beforeLockPublish = prev }
}

// AcquireServerStartWait is AcquireServerStart without the 1–60 minute range
// check, so tests can use a short wait.
func AcquireServerStartWait(ctx context.Context, adminDir, dataRoot string, wait time.Duration, logf func(string, ...any)) (*Server, error) {
	return acquireServerStart(ctx, adminDir, dataRoot, ServerOptions{AdminLockWait: wait, Logf: logf})
}
