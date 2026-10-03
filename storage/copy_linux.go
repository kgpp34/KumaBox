//go:build linux

package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// CopySparse copies data extents while preserving holes and the source's
// logical size. The destination must not already exist.
func CopySparse(destination, source string) (returnErr error) {
	input, err := os.Open(source) //nolint:gosec // callers supply validated managed paths
	if err != nil {
		return fmt.Errorf("open sparse source: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, input.Close()) }()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(err, errors.New("sparse source must be a regular file"))
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // managed staging path
	if err != nil {
		return fmt.Errorf("create sparse destination: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, output.Close()) }()
	if err := output.Truncate(info.Size()); err != nil {
		return err
	}
	for offset := int64(0); offset < info.Size(); {
		data, err := unix.Seek(int(input.Fd()), offset, unix.SEEK_DATA)
		if errors.Is(err, syscall.ENXIO) {
			break
		}
		if errors.Is(err, syscall.EINVAL) {
			return copyDense(output, input)
		}
		if err != nil {
			return fmt.Errorf("seek sparse data: %w", err)
		}
		hole, err := unix.Seek(int(input.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("seek sparse hole: %w", err)
		}
		if _, err := input.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := output.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(output, input, hole-data); err != nil {
			return fmt.Errorf("copy sparse extent: %w", err)
		}
		offset = hole
	}
	return output.Sync()
}

func copyDense(destination, source *os.File) error {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(destination, source); err != nil {
		return err
	}
	return destination.Sync()
}
