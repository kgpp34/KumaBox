//go:build !linux

package snapshot

import "os"

func sparseExtents(path string) ([]extent, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	if info.Size() == 0 {
		return nil, 0, nil
	}
	return []extent{{0, info.Size()}}, info.Size(), nil
}
