// Package version reports what build is running.
package version

import "fmt"

// Build information. Release builds override these through -ldflags.
var (
	Version   = "0.0.0-dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// String renders the version the way a human reads it.
func String() string {
	return fmt.Sprintf("kumabox %s (commit %s, built %s)", Version, Commit, BuildTime)
}

// Info returns the version as structured data for --json.
func Info() map[string]string {
	return map[string]string{
		"version":    Version,
		"commit":     Commit,
		"build_time": BuildTime,
	}
}
