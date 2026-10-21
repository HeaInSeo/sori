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

// SetBeforeServerLockCreate sets the hook that runs between finding no
// server.lock and creating it, and returns a function restoring the old hook.
func SetBeforeServerLockCreate(fn func()) (restore func()) {
	prev := beforeServerLockCreate
	beforeServerLockCreate = fn
	return func() { beforeServerLockCreate = prev }
}

// AcquireServerStartWait is AcquireServerStart without the 1–60 minute range
// check, so tests can use a short wait.
func AcquireServerStartWait(ctx context.Context, adminDir, dataRoot string, wait time.Duration, logf func(string, ...any)) (*Server, error) {
	return acquireServerStart(ctx, adminDir, dataRoot, ServerOptions{AdminLockWait: wait, Logf: logf})
}
