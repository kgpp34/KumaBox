//go:build !linux

package network

import (
	"fmt"
	"runtime"
)

func NetNSPath(vmID string) string {
	return vmID
}

func prepareCNINetnsLinux(_, _ string) (string, bool, error) {
	return "", false, fmt.Errorf("cni networking requires Linux (running on %s)", runtime.GOOS)
}

func setupCNIDatapathLinux(_, _, _ string, _ int, _ string) (string, error) {
	return "", fmt.Errorf("cni networking requires Linux (running on %s)", runtime.GOOS)
}

func deleteCNIDatapathLinux(_, _ string) error {
	return fmt.Errorf("cni networking requires Linux (running on %s)", runtime.GOOS)
}

func deleteCNINetnsLinux(_, _ string) error {
	return fmt.Errorf("cni networking requires Linux (running on %s)", runtime.GOOS)
}
