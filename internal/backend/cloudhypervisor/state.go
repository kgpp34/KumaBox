package cloudhypervisor

import (
	"context"

	"github.com/kumabox/kumabox/internal/vm"
)

// PauseVM pauses vCPU execution without terminating the VMM process.
func (Backend) PauseVM(ctx context.Context, rec *vm.VMRecord) error {
	return stateTransition(ctx, rec, apiVMPause, backendStatePaused)
}

// ResumeVM resumes a paused VM.
func (Backend) ResumeVM(ctx context.Context, rec *vm.VMRecord) error {
	return stateTransition(ctx, rec, apiVMResume, backendStateRunning)
}
