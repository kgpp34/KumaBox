//go:build linux

package storage

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// CloneFile creates an independent file. Filesystems with reflink support
// share unchanged extents; all other filesystems fall back to a sparse copy.
// The destination must not exist, and a failed reflink leaves no partial file.
func CloneFile(destination, source string) error {
	if err := reflinkFile(destination, source); err == nil {
		return nil
	}
	return CopySparse(destination, source)
}

func reflinkFile(destination, source string) (returnErr error) {
	input, err := os.Open(source) //nolint:gosec // caller supplies a managed artifact path
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, input.Close()) }()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(err, errors.New("clone source must be a regular file"))
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // managed destination
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, output.Close())
		if returnErr != nil {
			returnErr = errors.Join(returnErr, os.Remove(destination))
		}
	}()
	if err := unix.IoctlFileClone(int(output.Fd()), int(input.Fd())); err != nil {
		return fmt.Errorf("reflink source: %w", err)
	}
	return output.Sync()
}
