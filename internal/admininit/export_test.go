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

// SetAfterCheck installs a hook that Init runs after it checks the paths and
// before it takes the locks and opens either database, and returns a function
// restoring the previous one.
func SetAfterCheck(fn func()) (restore func()) {
	prev := afterCheck
	afterCheck = fn
	return func() { afterCheck = prev }
}
