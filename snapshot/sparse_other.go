//go:build !linux

package snapshot

import "os"

func scanSparse(_ *os.File, _ int64) ([]archiveExtent, bool, error) {
	return nil, false, nil
}
