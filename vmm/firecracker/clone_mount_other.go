//go:build !linux

package firecracker

import (
	"context"
	"errors"

	"github.com/kumabox/kumabox/vmm"
)

func (*Driver) launchCloneProcess(context.Context, processLaunch, *cloneSetup) (vmm.Process, error) {
	return vmm.Process{}, errors.New("firecracker clone requires Linux")
}

func verifyCloneDrives(int, []cloneDiskBinding) error {
	return errors.New("firecracker clone requires Linux")
}
