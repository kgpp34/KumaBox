//go:build linux

package disk

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/fileutil"
	"golang.org/x/sys/unix"
)

func probeReflink(directory string) (result bool, err error) {
	tmp, err := os.MkdirTemp(directory, ".kumabox-reflink-probe-")
	if err != nil {
		return false, fmt.Errorf("create reflink probe directory: %w", err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck

	sourcePath := filepath.Join(tmp, "source")
	destinationPath := filepath.Join(tmp, "destination")
	if err := os.WriteFile(sourcePath, []byte("kumabox-reflink-probe"), 0o600); err != nil {
		return false, fmt.Errorf("write reflink probe source: %w", err)
	}
	source, err := os.Open(sourcePath) //nolint:gosec
	if err != nil {
		return false, fmt.Errorf("open reflink probe source: %w", err)
	}
	defer fileutil.CloseAndJoin(&err, source, "close reflink probe source")
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec
	if err != nil {
		return false, fmt.Errorf("create reflink probe destination: %w", err)
	}
	defer fileutil.CloseAndJoin(&err, destination, "close reflink probe destination")

	if err := unix.IoctlFileClone(int(destination.Fd()), int(source.Fd())); err != nil {
		return false, nil
	}
	return true, nil
}
