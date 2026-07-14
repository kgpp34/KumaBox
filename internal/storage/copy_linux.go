//go:build linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func copyPlatform(ctx context.Context, source, destination string) (string, error) {
	src, err := os.Open(source) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("open source disk: %w", err)
	}
	defer src.Close()                                                            //nolint:errcheck
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("create destination disk: %w", err)
	}
	ok := false
	defer func() {
		_ = dst.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()

	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err == nil {
		ok = true
		return "reflink", nil
	}
	if err := copySparseExtents(ctx, src, dst); err == nil {
		ok = true
		return "sparse", nil
	} else if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.ENOSYS) {
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("close sparse fallback: %w", err)
	}
	if err := os.Remove(destination); err != nil {
		return "", fmt.Errorf("reset sparse fallback: %w", err)
	}
	strategy, err := bufferedCopy(ctx, source, destination)
	if err != nil {
		return "", err
	}
	ok = true
	return strategy, nil
}

func copySparseExtents(ctx context.Context, src, dst *os.File) error {
	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat source disk: %w", err)
	}
	if err := dst.Truncate(info.Size()); err != nil {
		return fmt.Errorf("size sparse disk: %w", err)
	}
	for offset := int64(0); offset < info.Size(); {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := unix.Seek(int(src.Fd()), offset, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			return nil
		}
		if err != nil {
			return err
		}
		hole, err := unix.Seek(int(src.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		if _, err := dst.Seek(data, io.SeekStart); err != nil {
			return fmt.Errorf("seek destination extent: %w", err)
		}
		if _, err := io.CopyN(dst, io.NewSectionReader(src, data, hole-data), hole-data); err != nil {
			return fmt.Errorf("copy sparse extent: %w", err)
		}
		offset = hole
	}
	return nil
}
