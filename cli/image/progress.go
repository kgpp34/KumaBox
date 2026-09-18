package image

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

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/types"
)

// imageProgress serializes terminal presentation and implements images.Reporter.
// Counts reflect completed work; the animation indicates activity, not a percentage.
// The owning command calls Finish after source and store cleanup.
//
//	start --> status/layer callbacks --> commit --> cleanup --> Finish
//	  |             |                                  |        |
//	  +--> ticker --+--> serialized stderr frames <-----+        |
//	         |                                                   |
//	         +<-- cancellation or stop <-------------------------+
//	               closes done; Finish joins before final output
type imageProgress struct {
	// mu protects mutable state and serializes ticker, callbacks, and result writes.
	mu sync.Mutex
	// writer receives progress on stderr, independently of command results on stdout.
	writer io.Writer
	// animated enables terminal redraws; redirected streams receive plain lines.
	animated bool
	// label identifies the operation and quoted image reference.
	label string
	// status describes the current stage, including waits without measurable progress.
	status string
	// completed counts successful callbacks, including out-of-order layer completions.
	completed int
	// total is the known layer or image count; zero means it is not yet available.
	total int
	// unit labels the count as layers or images.
	unit string
	// committed records that persistent state changed even if reporting later fails.
	committed bool
	// frame selects the next activity glyph without implying a completion percentage.
	frame int
	// err retains the first rendering failure so later callbacks cannot hide it.
	err error
	// stopOnce makes shutdown safe when command cleanup and test cleanup both join.
	stopOnce sync.Once
	// stop requests ticker shutdown; stopAnimation owns closing it.
	stop chan struct{}
	// done signals goroutine exit, or is closed immediately when animation is disabled.
	done chan struct{}
}

var _ images.Reporter = (*imageProgress)(nil)

// startImageProgress animates only actual terminal stderr, keeping redirected logs plain.
func startImageProgress(command *cobra.Command, operation, reference string) (*imageProgress, error) {
	writer := command.ErrOrStderr()
	file, ok := writer.(*os.File)
	animated := ok && isatty.IsTerminal(file.Fd())
	return newImageProgress(command.Context(), writer, fmt.Sprintf("%s %q", operation, reference), animated)
}

// newImageProgress writes the initial stage before starting any animation goroutine.
// If initialization fails, no goroutine or shutdown responsibility escapes to the caller.
func newImageProgress(ctx context.Context, writer io.Writer, label string, animated bool) (*imageProgress, error) {
	p := &imageProgress{
		writer: writer, animated: animated, label: label,
		status: "preparing image", unit: "layers", stop: make(chan struct{}), done: make(chan struct{}),
	}
	if err := p.render(); err != nil {
		return nil, err
	}
	if animated {
		go p.animate(ctx)
	} else {
		close(p.done)
	}
	return p, nil
}

// animate owns the ticker and closes done on cancellation, shutdown, or write failure.
// Each frame shares the same lock as worker callbacks and command result writes.
func (p *imageProgress) animate(ctx context.Context) {
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
			if p.err != nil {
				p.mu.Unlock()
				return
			}
			p.err = p.render()
			failed := p.err != nil
			p.mu.Unlock()
			if failed {
				return
			}
		}
	}
}

// Status updates the visible stage and returns any retained rendering failure.
func (p *imageProgress) Status(status string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.status = status
	p.err = p.render()
	return p.err
}

// Layer records a completed conversion; position is the zero-based source order.
// Completion count is independent of position because workers may finish out of order.
func (p *imageProgress) Layer(position, total int, digest types.Digest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.completed++
	p.total = total
	if p.completed == p.total {
		p.status = "publishing image"
	}
	if p.animated {
		p.err = p.render()
	} else {
		_, p.err = fmt.Fprintf(p.writer, "Layer %d/%d %s complete\n", position+1, total, digest.Hex()[:12])
	}
	return p.err
}

// Committed records durable import completion while keeping animation alive for cleanup.
// It preserves reporting failures so the command can distinguish committed-with-error state.
func (p *imageProgress) Committed(types.Image) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.committed = true
	p.status = "finishing"
	return p.err
}

// Removed records one successful deletion and switches the completion unit to images.
func (p *imageProgress) Removed(total int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.committed = true
	p.completed++
	p.total, p.unit = total, "images"
	p.status = "removing images"
	if p.err != nil {
		return p.err
	}
	if p.animated {
		p.err = p.render()
	}
	return p.err
}

// Output clears the current frame around result writes, then resumes the spinner.
func (p *imageProgress) Output(writer io.Writer) io.Writer {
	return progressOutput{progress: p, writer: writer}
}

// progressOutput coordinates result writes with an active stderr animation.
type progressOutput struct {
	// progress owns the shared rendering lock and current animation state.
	progress *imageProgress
	// writer receives result bytes unchanged, normally on stdout.
	writer io.Writer
}

// Write clears and restores the animation around one result write under the shared lock.
// It propagates both result-stream and redraw errors without changing result bytes.
func (w progressOutput) Write(data []byte) (int, error) {
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
	n, err := w.writer.Write(data)
	if p.animated {
		p.err = p.render()
	}
	return n, errors.Join(err, p.err)
}

// stopAnimation requests shutdown and joins without holding mu, avoiding ticker deadlock.
func (p *imageProgress) stopAnimation() {
	p.stopOnce.Do(func() { close(p.stop); <-p.done })
}

// Finish stops and joins the animation before writing the final line.
// The command calls it once, after its source and store cleanup have finished.
func (p *imageProgress) Finish(operationErr error) error {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	var classified *errdefs.Error
	if errors.As(operationErr, &classified) && classified.Committed {
		p.committed = true
	}
	result := "complete"
	symbol := "✓"
	if operationErr != nil || p.err != nil {
		result, symbol = "failed", "✗"
		if p.committed {
			result = "committed with errors"
		} else if errors.Is(operationErr, context.Canceled) {
			result = "canceled"
		}
	}
	message := fmt.Sprintf("%s %s", p.label, result)
	if p.total > 0 {
		message += fmt.Sprintf(" (%d/%d %s)", p.completed, p.total, p.unit)
	}
	if p.animated {
		message = "\r\x1b[2K" + symbol + " " + message
	}
	_, err := fmt.Fprintln(p.writer, message)
	return errdefs.Context(errors.Join(p.err, err), "image operation", p.label, "report", "check image state with image inspect", p.committed)
}

// render emits one terminal frame or plain stage line.
// The caller holds mu once the animation has started.
func (p *imageProgress) render() error {
	message := p.label + " · " + p.status
	if p.total > 0 {
		message += fmt.Sprintf(" (%d/%d %s)", p.completed, p.total, p.unit)
	}
	if p.animated {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		_, err := fmt.Fprintf(p.writer, "\r\x1b[2K%s %s", frames[p.frame%len(frames)], message)
		p.frame++
		return err
	}
	_, err := fmt.Fprintln(p.writer, message)
	return err
}
