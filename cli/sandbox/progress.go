package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// sandboxProgress serializes terminal animation, stage callbacks, and result output.
// Redirected stderr receives plain stage lines and stdout remains command data only.
type sandboxProgress struct {
	// mu serializes ticker, callback, and result output writes.
	mu sync.Mutex
	// writer receives progress independently of stdout command results.
	writer io.Writer
	// operation supplies error context such as create sandbox or remove sandbox.
	operation string
	// label identifies the operation and quoted user-facing reference.
	label string
	// status is the current application workflow stage.
	status string
	// recovery tells callers how to handle a progress rendering failure.
	recovery string
	// animated selects terminal redraws instead of plain log lines.
	animated bool
	// committed records that durable state changed despite a later failure.
	committed bool
	// frame indexes the next spinner glyph.
	frame int
	// err retains the first rendering failure.
	err error
	// stopOnce makes Finish safe if cleanup calls it more than once.
	stopOnce sync.Once
	// stop requests ticker shutdown.
	stop chan struct{}
	// done is closed after the ticker goroutine exits.
	done chan struct{}
}

var _ core.SandboxReporter = (*sandboxProgress)(nil)

// startCreateProgress starts progress for one create operation.
func startCreateProgress(command *cobra.Command, name string) (*sandboxProgress, error) {
	return startProgress(command, "create sandbox", fmt.Sprintf("Create %q", name), "preparing sandbox", "inspect the sandbox state")
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

// startProgress writes an initial stage before starting its ticker.
func startProgress(command *cobra.Command, operation, label, status, recovery string) (*sandboxProgress, error) {
	writer := command.ErrOrStderr()
	file, isFile := writer.(*os.File)
	progress := &sandboxProgress{
		writer: writer, operation: operation, label: label, status: status, recovery: recovery,
		animated: isFile && isatty.IsTerminal(file.Fd()), stop: make(chan struct{}), done: make(chan struct{}),
	}
	if err := progress.render(); err != nil {
		return nil, err
	}
	if progress.animated {
		go progress.animate(command.Context())
	} else {
		close(progress.done)
	}
	return progress, nil
}

// animate redraws until command cleanup finishes, cancellation occurs, or output fails.
func (p *sandboxProgress) animate(ctx context.Context) {
	defer close(p.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-ticker.C:
			p.mu.Lock()
			if p.err == nil {
				p.err = p.render()
			}
			failed := p.err != nil
			p.mu.Unlock()
			if failed {
				return
			}
		}
	}
}

// Status updates the current workflow stage.
func (p *sandboxProgress) Status(status string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.status = status
	p.err = p.render()
	return p.err
}

// Committed records that durable application state changed before reporting finished.
func (p *sandboxProgress) Committed(types.Sandbox) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.committed = true
	p.status = "finishing"
	return p.err
}

// Output coordinates stdout writes with terminal redraws.
func (p *sandboxProgress) Output(writer io.Writer) io.Writer {
	return progressWriter{progress: p, writer: writer}
}

// progressWriter prevents a live animation from visually mixing with command output.
type progressWriter struct {
	// progress owns output serialization and animation state.
	progress *sandboxProgress
	// writer receives the unchanged command result.
	writer io.Writer
}

func (w progressWriter) Write(data []byte) (int, error) {
	p := w.progress
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return 0, p.err
	}
	if p.animated {
		if _, err := fmt.Fprint(p.writer, "\r\x1b[2K"); err != nil {
			p.err = err
			return 0, err
		}
	}
	n, writeErr := w.writer.Write(data)
	if p.animated {
		p.err = p.render()
	}
	return n, errors.Join(writeErr, p.err)
}

// Finish joins the ticker and emits one unambiguous final status line.
func (p *sandboxProgress) Finish(operationErr error) error {
	p.stopOnce.Do(func() { close(p.stop); <-p.done })
	p.mu.Lock()
	defer p.mu.Unlock()
	var classified *errdefs.Error
	if errors.As(operationErr, &classified) && classified.Committed {
		p.committed = true
	}
	resultText, symbol := "complete", "✓"
	if operationErr != nil || p.err != nil {
		resultText, symbol = "failed", "✗"
		if p.committed {
			resultText = "committed with errors"
		} else if errors.Is(operationErr, context.Canceled) {
			resultText = "canceled"
		}
	}
	message := fmt.Sprintf("%s %s", p.label, resultText)
	if p.animated {
		message = "\r\x1b[2K" + symbol + " " + message
	}
	_, err := fmt.Fprintln(p.writer, message)
	return errdefs.Context(errors.Join(p.err, err), p.operation, p.label, "report", p.recovery, p.committed)
}

// render writes one spinner frame or one plain stage line. The caller holds mu
// after animation starts.
func (p *sandboxProgress) render() error {
	message := p.label + " · " + p.status
	if p.animated {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		_, err := fmt.Fprintf(p.writer, "\r\x1b[2K%s %s", frames[p.frame%len(frames)], message)
		p.frame++
		return err
	}
	_, err := fmt.Fprintln(p.writer, message)
	return err
}
