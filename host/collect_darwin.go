//go:build darwin

package host

import "syscall"

// kernelVersion reports the running XNU version.
func kernelVersion() string {
	version, err := syscall.Sysctl("kern.osrelease")
	if err != nil {
		return ""
	}
	return version
}
