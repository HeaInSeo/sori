package admininit

// SetAfterStep installs a hook that runs after each durable step and returns a
// function restoring the previous one.
func SetAfterStep(fn func(Step) error) (restore func()) {
	prev := afterStep
	afterStep = fn
	return func() { afterStep = prev }
}

// SetAfterCheck installs a hook that Init runs after it checks the paths and
// before it takes the locks and opens either database, and returns a function
// restoring the previous one.
func SetAfterCheck(fn func()) (restore func()) {
	prev := afterCheck
	afterCheck = fn
	return func() { afterCheck = prev }
}
