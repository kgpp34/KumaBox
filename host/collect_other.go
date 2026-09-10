//go:build !linux && !darwin

package host

// kernelVersion is unavailable on this platform. Every Linux-only requirement
// is already reported as unsupported here.
func kernelVersion() string { return "" }
