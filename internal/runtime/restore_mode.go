package runtime

import (
	"fmt"
	"slices"

	"github.com/kumabox/kumabox/internal/backend"
)

const (
	restoreModeCopy     = "copy"
	restoreModeOnDemand = "ondemand"
	restoreModeMmap     = "mmap"
)

func normalizeRestoreMode(mode string) (string, error) {
	if mode == "" {
		return restoreModeCopy, nil
	}
	switch mode {
	case restoreModeCopy, restoreModeOnDemand, restoreModeMmap:
		return mode, nil
	default:
		return "", fmt.Errorf("RESTORE_MODE_UNSUPPORTED: %s", mode)
	}
}

func requireRestoreMode(host backend.NativeHost, mode string) error {
	if mode == restoreModeCopy {
		return nil
	}
	if slices.Contains(host.RestoreModes, mode) {
		return nil
	}
	return fmt.Errorf(
		"RESTORE_MODE_UNSUPPORTED: cloud-hypervisor %s does not advertise %s memory restore",
		host.BackendVersion,
		mode,
	)
}

func restoreModePinsSnapshot(mode string) bool {
	return mode == restoreModeOnDemand || mode == restoreModeMmap
}
