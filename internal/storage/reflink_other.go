//go:build !linux

package storage

func probeReflink(string) (bool, error) {
	return false, nil
}
