//go:build !linux

package daemon

// Other hosts use the reconciliation ticker without Linux process exit hints.
func newExitWatcher() (exitWatcher, error) { return nil, nil }
