//go:build linux

package snapshot

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// scanSparse locates allocated extents without reading logical holes. A file
// system without SEEK_DATA/SEEK_HOLE uses the ordinary dense tar path.
func scanSparse(file *os.File, size int64) ([]archiveExtent, bool, error) {
	if size == 0 {
		return nil, false, nil
	}
	var extents []archiveExtent
	for offset := int64(0); offset < size; {
		data, err := unix.Seek(int(file.Fd()), offset, unix.SEEK_DATA)
		if errors.Is(err, syscall.ENXIO) {
			break
		}
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("scan sparse data: %w", err)
		}
		hole, err := unix.Seek(int(file.Fd()), data, unix.SEEK_HOLE)
		switch {
		case errors.Is(err, syscall.ENXIO):
			hole = size
		case errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP):
			return nil, false, nil
		case err != nil:
			return nil, false, fmt.Errorf("scan sparse hole: %w", err)
		}
		if hole > size {
			hole = size
		}
		if hole <= data {
			return nil, false, errors.New("invalid sparse extent returned by filesystem")
		}
		extents = append(extents, archiveExtent{Offset: data, Length: hole - data})
		offset = hole
	}
	var allocated int64
	for _, extent := range extents {
		allocated += extent.Length
	}
	return extents, allocated < size, nil
}
