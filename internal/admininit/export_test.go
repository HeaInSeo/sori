package admininit

// SetAfterStep installs a hook that runs after each durable step and returns a
// function restoring the previous one.
func SetAfterStep(fn func(Step) error) (restore func()) {
	prev := afterStep
	afterStep = fn
	return func() { afterStep = prev }
}
