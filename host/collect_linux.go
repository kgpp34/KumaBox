//go:build linux

package host

import (
	"os"
	"strings"
)

// kernelVersion reads the running kernel release from /proc.
func kernelVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
