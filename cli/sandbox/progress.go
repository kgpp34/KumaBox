package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/spf13/cobra"

	cliprogress "github.com/kumabox/kumabox/cli/progress"
	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// sandboxProgress adapts SandboxService stages and commit events to the shared
// renderer while retaining operation-specific recovery context.
type sandboxProgress struct {
	// mu protects stage and commit state across reporter and final callbacks.
	mu sync.Mutex
	// renderer owns terminal detection, serialization, animation, and shutdown.
	renderer *cliprogress.Renderer
	// operation supplies structured error context.
	operation string
	// label identifies the operation and quoted sandbox reference.
	label string
	// status is the current application workflow stage.
	status string
	// recovery describes how to inspect or retry a reporting failure.
	recovery string
	// committed records durable state changed before a later error.
	committed bool
}

var _ core.SandboxReporter = (*sandboxProgress)(nil)

// startCreateProgress starts progress for one create operation.
func startCreateProgress(command *cobra.Command, name string) (*sandboxProgress, error) {
	return startProgress(command, "create sandbox", fmt.Sprintf("Create %q", name), "preparing sandbox", "inspect the sandbox state")
}

// startRunProgress starts progress for one create-and-launch operation.
func startRunProgress(command *cobra.Command, name string) (*sandboxProgress, error) {
	return startProgress(command, "run sandbox", fmt.Sprintf("Run %q", name), "preparing sandbox", "inspect the sandbox state and VMM log")
}

// startRemoveProgress starts progress for one remove operation.
func startRemoveProgress(command *cobra.Command, reference string) (*sandboxProgress, error) {
	return startProgress(command, "remove sandbox", fmt.Sprintf("Remove %q", reference), "preparing removal", "retry removal or inspect retained state")
}

// startStartProgress starts progress for one VMM launch operation.
func startStartProgress(command *cobra.Command, reference string) (*sandboxProgress, error) {
	return startProgress(command, "start sandbox", fmt.Sprintf("Start %q", reference), "preparing start", "inspect the sandbox state and VMM log")
}

// startStopProgress starts progress for one controlled VMM termination.
func startStopProgress(command *cobra.Command, reference string) (*sandboxProgress, error) {
	return startProgress(command, "stop sandbox", fmt.Sprintf("Stop %q", reference), "preparing stop", "retry the stop or inspect the sandbox runtime")
}

// snapshotStatusProgress adapts snapshot-service status callbacks while using
// the sandbox renderer for restore output and failure semantics.
type snapshotStatusProgress struct{ *sandboxProgress }

// Committed records a saved snapshot if a shared snapshot workflow emits one.
func (p *snapshotStatusProgress) Committed(types.Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.committed = true
	return p.renderer.Err()
}

// startRestoreProgress starts progress for one native snapshot restore.
func startRestoreProgress(command *cobra.Command, reference string) (*snapshotStatusProgress, error) {
	progress, err := startProgress(command, "restore sandbox", fmt.Sprintf("Restore %q", reference), "preparing restore", "inspect the sandbox state and VMM log")
	if err != nil {
		return nil, err
	}
	return &snapshotStatusProgress{sandboxProgress: progress}, nil
}

func startProgress(command *cobra.Command, operation, label, status, recovery string) (*sandboxProgress, error) {
	return newSandboxProgress(command.Context(), command.ErrOrStderr(), operation, label, status, recovery)
}

// newSandboxProgress builds the domain adapter and writes its initial status.
func newSandboxProgress(ctx context.Context, writer io.Writer, operation, label, status, recovery string) (*sandboxProgress, error) {
	renderer, err := cliprogress.New(ctx, writer, label+" · "+status)
	if err != nil {
		return nil, err
	}
	return &sandboxProgress{
		renderer: renderer, operation: operation, label: label, status: status, recovery: recovery,
	}, nil
}

// Status updates the current sandbox workflow stage.
func (p *sandboxProgress) Status(status string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = status
	return p.renderer.Update(p.label + " · " + status)
}

// Committed records a durable sandbox state change before cleanup completes.
func (p *sandboxProgress) Committed(types.Sandbox) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.committed = true
	p.status = "finishing"
	if p.renderer.Animated() {
		return p.renderer.Update(p.label + " · " + p.status)
	}
	return p.renderer.Err()
}

// Output coordinates command results with a live terminal frame.
func (p *sandboxProgress) Output(writer io.Writer) io.Writer {
	return p.renderer.Output(writer)
}

// Finish maps sandbox commit and cancellation facts to a generic final outcome.
func (p *sandboxProgress) Finish(operationErr error) error {
	p.mu.Lock()
	var classified *errdefs.Error
	if errors.As(operationErr, &classified) && classified.Committed {
		p.committed = true
	}
	renderErr := p.renderer.Err()
	outcome := cliprogress.Succeeded
	if operationErr != nil || renderErr != nil {
		switch {
		case p.committed:
			outcome = cliprogress.CommittedWithErrors
		case errors.Is(operationErr, context.Canceled):
			outcome = cliprogress.Canceled
		default:
			outcome = cliprogress.Failed
		}
	}
	committed, operation, label, recovery := p.committed, p.operation, p.label, p.recovery
	p.mu.Unlock()

	reportErr := p.renderer.Finish(label, outcome, "")
	return errdefs.Context(reportErr, operation, label, "report", recovery, committed)
}
