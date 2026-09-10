// Package version reports what build is running.
package version

import (
	"encoding/json"
	"fmt"
	"io"
)

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

// WriteJSON writes the version fields as JSON.
func WriteJSON(out io.Writer) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(map[string]string{
		"version":    Version,
		"commit":     Commit,
		"build_time": BuildTime,
	})
}
