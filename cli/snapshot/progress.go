package snapshot

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/spf13/cobra"

	cliprogress "github.com/kumabox/kumabox/cli/progress"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

type progress struct {
	mu        sync.Mutex
	renderer  *cliprogress.Renderer
	label     string
	committed bool
}

func newProgress(command *cobra.Command, reference string) (*progress, error) {
	label := fmt.Sprintf("Snapshot %q", reference)
	renderer, err := cliprogress.New(command.Context(), command.ErrOrStderr(), label+" · preparing snapshot")
	if err != nil {
		return nil, err
	}
	return &progress{renderer: renderer, label: label}, nil
}

func (p *progress) Status(status string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.renderer.Update(p.label + " · " + status)
}

func (p *progress) Committed(types.Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.committed = true
	return p.renderer.Update(p.label + " · finishing")
}

func (p *progress) Output(writer io.Writer) io.Writer { return p.renderer.Output(writer) }

func (p *progress) Finish(operationErr error) error {
	p.mu.Lock()
	var classified *errdefs.Error
	if errors.As(operationErr, &classified) && classified.Committed {
		p.committed = true
	}
	outcome := cliprogress.Succeeded
	if operationErr != nil || p.renderer.Err() != nil {
		if p.committed {
			outcome = cliprogress.CommittedWithErrors
		} else {
			outcome = cliprogress.Failed
		}
	}
	label := p.label
	p.mu.Unlock()
	return p.renderer.Finish(label, outcome, "")
}
