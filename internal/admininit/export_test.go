package admininit

// SetAfterStep installs a hook that runs after each durable step and returns a
// function restoring the previous one.
func SetAfterStep(fn func(Step) error) (restore func()) {
	prev := afterStep
	afterStep = fn
	return func() { afterStep = prev }
}

// SetBeforeOpen installs a hook that runs before each durable step opens a
// database, and returns a function restoring the previous one.
func SetBeforeOpen(fn func(Step)) (restore func()) {
	prev := beforeOpen
	beforeOpen = fn
	return func() { beforeOpen = prev }
}

// SetBeforeStep installs a hook that runs after a durable step's first check and
// before the step opens its database by path, and returns a function restoring
// the previous one.
func SetBeforeStep(fn func(Step)) (restore func()) {
	prev := beforeStep
	beforeStep = fn
	return func() { beforeStep = prev }
}

// SetAfterSee installs a hook that runs after ProvisionAndBootstrap records the
// database paths and before the preflight inspects them, and returns a function
// restoring the previous one.
func SetAfterSee(fn func()) (restore func()) {
	prev := afterSee
	afterSee = fn
	return func() { afterSee = prev }
}

// SetTestTrust makes uid the init principal and gid the group sori, accepts
// ancestors owned by ancestorUID and stops the ancestor walk below stop. It
// returns a function restoring the previous values.
func SetTestTrust(uid, gid, ancestorUID uint32, stop string) (restore func()) {
	prevLookup, prevOwner, prevStop := lookupTrust, ancestorOwner, ancestorStop
	lookupTrust = func() (trust, error) { return trust{uid: uid, gid: gid}, nil }
	ancestorOwner, ancestorStop = ancestorUID, stop
	return func() { lookupTrust, ancestorOwner, ancestorStop = prevLookup, prevOwner, prevStop }
}

// ProductionAncestors restores the production ancestor rule (owner root, walk to
// "/") and returns a function undoing that.
func ProductionAncestors() (restore func()) {
	prevOwner, prevStop := ancestorOwner, ancestorStop
	ancestorOwner, ancestorStop = 0, ""
	return func() { ancestorOwner, ancestorStop = prevOwner, prevStop }
}

// SetUseGeneration makes the identity tuple use or ignore the inode generation
// and returns a function restoring the previous setting.
func SetUseGeneration(on bool) (restore func()) {
	prev := useGeneration
	useGeneration = on
	return func() { useGeneration = prev }
}

// SetTickWait installs a hook that sees each clock reading (seconds and
// nanoseconds) of Init's wait past a created file's birth time and may replace
// it. It returns a function restoring the previous hook.
func SetTickWait(fn func(btimeSec int64, btimeNsec uint32, nowSec int64, nowNsec uint32) (int64, uint32)) (restore func()) {
	prev := onTickWait
	onTickWait = func(t, now stamp) stamp {
		s, n := fn(t.Sec, t.Nsec, now.Sec, now.Nsec)
		return stamp{Sec: s, Nsec: n}
	}
	return func() { onTickWait = prev }
}

// SetBeforeClose installs a hook that runs after a created database's record is
// durable and before Init closes its descriptor.
func SetBeforeClose(fn func(Step)) (restore func()) {
	prev := beforeClose
	beforeClose = fn
	return func() { beforeClose = prev }
}

// SetAfterCheck installs a hook that Init runs after it checks the paths and
// before it takes the locks and opens either database, and returns a function
// restoring the previous one.
func SetAfterCheck(fn func()) (restore func()) {
	prev := afterCheck
	afterCheck = fn
	return func() { afterCheck = prev }
}
