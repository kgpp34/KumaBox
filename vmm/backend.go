package vmm

import (
	"context"
	"io"

	"github.com/kumabox/kumabox/types"
)

// Backend is the stable process-level contract implemented by every VMM
// adapter. Durable sandbox transitions remain in core; implementations own
// process launch, identity, control APIs, console access, and runtime cleanup.
type Backend interface {
	Type() types.VMMType
	Preflight() error
	Locate(context.Context, types.SandboxID, uint64) (Process, bool, error)
	Observe(context.Context, types.SandboxID, uint64) (Observation, error)
	WaitReady(context.Context, Process) error
	Launch(context.Context, LaunchPlan) (Process, error)
	Abort(context.Context, Process) error
	Stop(context.Context, Process) error
	Console(context.Context, Process) (io.ReadWriteCloser, error)
	Cleanup(context.Context, types.SandboxID) error
}
