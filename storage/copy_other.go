//go:build !linux

package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// CopySparse provides a portable development-host fallback. Production Linux
// builds use extent-aware copying to preserve holes.
func CopySparse(destination, source string) (returnErr error) {
	input, err := os.Open(source) //nolint:gosec // callers supply validated managed paths
	if err != nil {
		return fmt.Errorf("open sparse source: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, input.Close()) }()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // managed staging path
	if err != nil {
		return fmt.Errorf("create sparse destination: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, output.Close()) }()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	return output.Sync()
}
