package core

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// validateCapturedDataDisks checks the portable file contract before a clone
// creates an identity or a restore stops the current VM.
func validateCapturedDataDisks(directory string, specs []types.DataDiskSpec) error {
	for _, spec := range specs {
		path := filepath.Join(directory, types.DataDiskFile(spec.Name))
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect captured data disk %q: %w", spec.Name, err)
		}
		if !info.Mode().IsRegular() || info.Size() != spec.Size {
			return fmt.Errorf("captured data disk %q is not a regular %d-byte file", spec.Name, spec.Size)
		}
	}
	return nil
}

// cloneDiskBindings splits the validated launch attachment order at the COW.
// Clone rewrites immutable layer paths separately from writable data paths.
func cloneDiskBindings(plan vmm.LaunchPlan, dataCount int) ([]vmm.Disk, []vmm.Disk, error) {
	if dataCount < 0 || len(plan.Disks) < dataCount+2 || plan.Disks[len(plan.Disks)-dataCount-1].Serial != vmm.COWSerial {
		return nil, nil, fmt.Errorf("launch disk order does not match %d data disks", dataCount)
	}
	return plan.Disks[:len(plan.Disks)-dataCount-1], plan.Disks[len(plan.Disks)-dataCount:], nil
}
