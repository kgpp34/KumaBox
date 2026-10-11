// Package progress renders serialized CLI activity without knowing the domain
// event that produced each message.
package progress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

const frameInterval = 100 * time.Millisecond

var (
	spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	errFinished   = errors.New("progress renderer is already finished")
)

// Outcome selects the final status text and terminal symbol.
type Outcome uint8

const (
	_ Outcome = iota
	// Succeeded reports that the operation and its cleanup completed.
	Succeeded
	// Failed reports that the operation made no known durable change.
	Failed
	// Canceled reports context cancellation before a durable change.
	Canceled
	// CommittedWithErrors reports a durable change followed by an error.
	CommittedWithErrors
)

// Renderer serializes animation, status changes, result output, and shutdown.
// Domain adapters supply already formatted messages and never access terminal
// state directly.
//
//	New -> initial frame -> Update / Output -> Finish
//	          |                ^             |
//	          +---- ticker ----+---- stop ---+
//	                    context cancellation
type Renderer struct {
	// mu serializes ticker, adapter, result, and final writes.
	mu sync.Mutex
	// writer receives progress independently of command results.
	writer io.Writer
	// animated selects terminal redraws instead of durable plain lines.
	animated bool
	// message is the complete current activity text supplied by an adapter.
	message string
	// lastPlain suppresses consecutive duplicate statuses in redirected logs.
	lastPlain string
	// frame selects the next spinner glyph.
	frame int
	// err retains the first progress-stream failure.
	err error
	// finished prevents writes after the terminal result line.
	finished bool
	// stopOnce makes joining safe after cancellation or a rendering failure.
	stopOnce sync.Once
	// stop requests animation shutdown.
	stop chan struct{}
	// done closes after the animation goroutine and ticker have exited.
	done chan struct{}
}

type tickerFactory func(time.Duration) (<-chan time.Time, func())

// New writes the initial status and animates only when writer is a terminal.
func New(ctx context.Context, writer io.Writer, initial string) (*Renderer, error) {
	file, isFile := writer.(*os.File)
	animated := isFile && isatty.IsTerminal(file.Fd())
	return newRenderer(ctx, writer, initial, animated, systemTicker)
}

// newRenderer accepts a ticker factory so tests can advance animation without
// wall-clock sleeps. Production construction always uses systemTicker.
func newRenderer(ctx context.Context, writer io.Writer, initial string, animated bool, ticker tickerFactory) (*Renderer, error) {
	if ctx == nil {
		return nil, errors.New("progress context must not be nil")
	}
	if writer == nil {
		return nil, errors.New("progress writer must not be nil")
	}
	if initial == "" {
		return nil, errors.New("initial progress message must not be empty")
	}
	renderer := &Renderer{
		writer: writer, animated: animated, message: initial,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	if err := renderer.renderLocked(); err != nil {
		return nil, err
	}
	if !animated {
		close(renderer.done)
		return renderer, nil
	}
	ticks, stopTicker := ticker(frameInterval)
	go renderer.animate(ctx, ticks, stopTicker)
	return renderer, nil
}

func systemTicker(interval time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

// Animated reports whether the renderer redraws one terminal line.
func (r *Renderer) Animated() bool { return r.animated }

// Err returns the first rendering error, if any.
func (r *Renderer) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Update replaces the current terminal frame. Plain streams print each
// distinct status once, so repeated callbacks cannot flood redirected logs.
func (r *Renderer) Update(message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return errFinished
	}
	if r.err != nil {
		return r.err
	}
	r.message = message
	r.err = r.renderLocked()
	return r.err
}

// Output wraps a command-result writer. A live terminal frame is cleared
// before the result write and restored afterward under the renderer lock.
func (r *Renderer) Output(writer io.Writer) io.Writer {
	return outputWriter{renderer: r, writer: writer}
}

type outputWriter struct {
	// renderer owns serialization and the progress stream.
	renderer *Renderer
	// writer receives command result bytes unchanged.
	writer io.Writer
}

func (w outputWriter) Write(data []byte) (int, error) {
	renderer := w.renderer
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if renderer.finished {
		return 0, errFinished
	}
	if renderer.err != nil {
		return 0, renderer.err
	}
	if renderer.animated {
		if _, err := fmt.Fprint(renderer.writer, "\r\x1b[2K"); err != nil {
			renderer.err = err
			return 0, err
		}
	}
	written, writeErr := w.writer.Write(data)
	if renderer.animated {
		renderer.err = renderer.renderLocked()
	}
	return written, errors.Join(writeErr, renderer.err)
}

// Finish joins the animation before emitting one newline-terminated result.
func (r *Renderer) Finish(label string, outcome Outcome, detail string) error {
	r.stopAnimation()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return errFinished
	}
	r.finished = true
	if r.err != nil && outcome == Succeeded {
		outcome = Failed
	}
	text, symbol, err := outcomePresentation(outcome)
	if err != nil {
		return errors.Join(r.err, err)
	}
	message := label + " " + text + detail
	if r.animated {
		message = "\r\x1b[2K" + symbol + " " + message
	}
	_, writeErr := fmt.Fprintln(r.writer, message)
	return errors.Join(r.err, writeErr)
}

func outcomePresentation(outcome Outcome) (string, string, error) {
	switch outcome {
	case Succeeded:
		return "complete", "✓", nil
	case Failed:
		return "failed", "✗", nil
	case Canceled:
		return "canceled", "✗", nil
	case CommittedWithErrors:
		return "committed with errors", "✗", nil
	default:
		return "", "", fmt.Errorf("invalid progress outcome %d", outcome)
	}
}

func (r *Renderer) animate(ctx context.Context, ticks <-chan time.Time, stopTicker func()) {
	defer close(r.done)
	defer stopTicker()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-ticks:
			r.mu.Lock()
			if r.err == nil && !r.finished {
				r.err = r.renderLocked()
			}
			failed := r.err != nil || r.finished
			r.mu.Unlock()
			if failed {
				return
			}
		}
	}
}

// stopAnimation requests shutdown and waits without holding mu, which lets an
// in-flight ticker write finish before the final line is emitted.
func (r *Renderer) stopAnimation() {
	r.stopOnce.Do(func() {
		close(r.stop)
		<-r.done
	})
}

// renderLocked emits one frame or one deduplicated plain line. The caller
// holds mu whenever the renderer is visible to another goroutine.
func (r *Renderer) renderLocked() error {
	if r.animated {
		_, err := fmt.Fprintf(r.writer, "\r\x1b[2K%s %s", spinnerFrames[r.frame%len(spinnerFrames)], r.message)
		r.frame++
		return err
	}
	if r.message == r.lastPlain {
		return nil
	}
	if _, err := fmt.Fprintln(r.writer, r.message); err != nil {
		return err
	}
	r.lastPlain = r.message
	return nil
}
