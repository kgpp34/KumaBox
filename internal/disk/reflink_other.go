//go:build !linux

package disk

func probeReflink(string) (bool, error) {
	return false, nil
}
