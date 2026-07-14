package cloudhypervisor

import (
	"context"

	"github.com/kumabox/kumabox/internal/vmstore"
)

// PauseVM pauses vCPU execution without terminating the VMM process.
func (Backend) PauseVM(ctx context.Context, rec *vmstore.VMRecord) error {
	return stateTransition(ctx, rec, "vm.pause", "Paused")
}

// ResumeVM resumes a paused VM.
func (Backend) ResumeVM(ctx context.Context, rec *vmstore.VMRecord) error {
	return stateTransition(ctx, rec, "vm.resume", "Running")
}
