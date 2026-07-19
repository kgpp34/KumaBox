package runtime

import (
	"fmt"
	"slices"

	"github.com/kumabox/kumabox/internal/backend"
)

type RestoreMode string

const (
	RestoreModeCopy     RestoreMode = "copy"
	RestoreModeOnDemand RestoreMode = "ondemand"
	RestoreModeMmap     RestoreMode = "mmap"
)

func normalizeRestoreMode(mode RestoreMode) (RestoreMode, error) {
	if mode == "" {
		return RestoreModeCopy, nil
	}
	switch mode {
	case RestoreModeCopy, RestoreModeOnDemand, RestoreModeMmap:
		return mode, nil
	default:
		return "", fmt.Errorf("RESTORE_MODE_UNSUPPORTED: %s", mode)
	}
}

func requireRestoreMode(host backend.NativeHost, mode RestoreMode) error {
	if mode == RestoreModeCopy {
		return nil
	}
	if slices.Contains(host.RestoreModes, string(mode)) {
		return nil
	}
	return fmt.Errorf(
		"RESTORE_MODE_UNSUPPORTED: cloud-hypervisor %s does not advertise %s memory restore",
		host.BackendVersion,
		mode,
	)
}

func restoreModePinsSnapshot(mode RestoreMode) bool {
	return mode == RestoreModeOnDemand || mode == RestoreModeMmap
}
