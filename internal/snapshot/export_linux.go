//go:build linux

package snapshot

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"

	"github.com/kumabox/kumabox/internal/fileutil"
)

func sparseExtents(path string) (result []extent, size int64, err error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, 0, fmt.Errorf("open sparse disk: %w", err)
	}
	defer fileutil.CloseAndJoin(&err, f, "close sparse disk")
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	var extents []extent
	for off := int64(0); off < info.Size(); {
		data, err := unix.Seek(int(f.Fd()), off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			return []extent{{0, info.Size()}}, info.Size(), nil
		}
		hole, err := unix.Seek(int(f.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			return nil, 0, err
		}
		extents = append(extents, extent{data, hole - data})
		off = hole
	}
	return extents, info.Size(), nil
}
