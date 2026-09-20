package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/spf13/cobra"

	cliprogress "github.com/kumabox/kumabox/cli/progress"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/types"
)

// imageProgress adapts image-specific counters and commit events to the shared
// terminal renderer. Counts represent completed work rather than percentage.
//
//	images.Reporter callbacks -> image message/counters -> progress.Renderer
type imageProgress struct {
	// mu protects counters, status, and commit state across worker callbacks.
	mu sync.Mutex
	// renderer owns terminal detection, serialization, animation, and shutdown.
	renderer *cliprogress.Renderer
	// label identifies the operation and quoted image reference.
	label string
	// status describes the current image workflow stage.
	status string
	// completed and total count successful layer or image callbacks.
	completed int
	total     int
	// unit labels the counter as layers or images.
	unit string
	// committed records durable state changed before a later error.
	committed bool
}

var _ images.Reporter = (*imageProgress)(nil)

// startImageProgress creates the image adapter on the command's stderr stream.
func startImageProgress(command *cobra.Command, operation, reference string) (*imageProgress, error) {
	return newImageProgress(command.Context(), command.ErrOrStderr(), fmt.Sprintf("%s %q", operation, reference))
}

// newImageProgress builds the domain adapter and writes its initial status.
func newImageProgress(ctx context.Context, writer io.Writer, label string) (*imageProgress, error) {
	status := "preparing image"
	renderer, err := cliprogress.New(ctx, writer, label+" · "+status)
	if err != nil {
		return nil, err
	}
	return &imageProgress{renderer: renderer, label: label, status: status, unit: "layers"}, nil
}

// Status updates the visible image workflow stage.
func (p *imageProgress) Status(status string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = status
	return p.renderer.Update(p.messageLocked())
}

// Layer records an out-of-order layer completion. Redirected output receives
// one durable line per layer while terminals redraw the aggregate counter.
func (p *imageProgress) Layer(position, total int, digest types.Digest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completed++
	p.total = total
	if p.completed == p.total {
		p.status = "publishing image"
	}
	if p.renderer.Animated() {
		return p.renderer.Update(p.messageLocked())
	}
	return p.renderer.Update(fmt.Sprintf("Layer %d/%d %s complete", position+1, total, digest.Hex()[:12]))
}

// Committed records durable image publication before source and store cleanup.
func (p *imageProgress) Committed(types.Image) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.committed = true
	p.status = "finishing"
	if p.renderer.Animated() {
		return p.renderer.Update(p.messageLocked())
	}
	return p.renderer.Err()
}

// Removed records one successful image deletion.
func (p *imageProgress) Removed(total int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.committed = true
	p.completed++
	p.total, p.unit = total, "images"
	p.status = "removing images"
	if p.renderer.Animated() {
		return p.renderer.Update(p.messageLocked())
	}
	return p.renderer.Err()
}

// Output coordinates command results with a live terminal frame.
func (p *imageProgress) Output(writer io.Writer) io.Writer {
	return p.renderer.Output(writer)
}

// Finish maps image commit and cancellation facts to a generic final outcome.
func (p *imageProgress) Finish(operationErr error) error {
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
	detail := ""
	if p.total > 0 {
		detail = fmt.Sprintf(" (%d/%d %s)", p.completed, p.total, p.unit)
	}
	committed, label := p.committed, p.label
	p.mu.Unlock()

	reportErr := p.renderer.Finish(label, outcome, detail)
	return errdefs.Context(reportErr, "image operation", label, "report", "check image state with image inspect", committed)
}

// messageLocked formats aggregate image state while p.mu is held.
func (p *imageProgress) messageLocked() string {
	message := p.label + " · " + p.status
	if p.total > 0 {
		message += fmt.Sprintf(" (%d/%d %s)", p.completed, p.total, p.unit)
	}
	return message
}
