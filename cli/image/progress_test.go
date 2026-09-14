package image

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
)

func TestProgressLogsHaveNoAnimationControls(t *testing.T) {
	var out bytes.Buffer
	p, err := newImageProgress(t.Context(), &out, `Import "demo"`, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Status("converting layers"); err != nil {
		t.Fatal(err)
	}
	if err := p.Layer(1, 2, images.Digest{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Layer(0, 2, images.Digest{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Committed(images.Image{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\r\x1b") || !strings.Contains(out.String(), "complete (2/2 layers)\n") {
		t.Fatalf("plain progress = %q", out.String())
	}
	if !strings.Contains(out.String(), "preparing image") || strings.Count(out.String(), " complete\n") != 2 {
		t.Fatalf("missing initial status or layer notifications: %s", out.String())
	}
}

// Observe real ticker writes without sleeping or reading a buffer concurrently.
type observedProgressWriter struct {
	mu      sync.Mutex
	out     bytes.Buffer
	writes  int
	changed chan struct{}
	failAt  int
	failure error
}

func (w *observedProgressWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	select {
	case w.changed <- struct{}{}:
	default:
	}
	if w.writes == w.failAt {
		return 0, w.failure
	}
	return w.out.Write(data)
}

func (w *observedProgressWriter) snapshot() (string, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.String(), w.writes
}

func waitProgressDone(t *testing.T, p *imageProgress) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("animation did not stop")
	}
}

func TestProgressAnimatesAndCountsConcurrentCompletedLayers(t *testing.T) {
	writer := &observedProgressWriter{changed: make(chan struct{}, 1)}
	p, err := newImageProgress(t.Context(), writer, `Import "demo"`, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stopAnimation)
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		if _, writes := writer.snapshot(); writes >= 2 {
			break
		}
		select {
		case <-writer.changed:
		case <-timeout.C:
			t.Fatal("spinner did not advance while waiting")
		}
	}
	if err := p.Status("converting layers"); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for _, position := range []int{2, 0, 1} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := p.Layer(position, 3, images.Digest{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	if err := p.Committed(images.Image{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(nil); err != nil {
		t.Fatal(err)
	}
	out, _ := writer.snapshot()
	if !strings.Contains(out, "⠋") || !strings.Contains(out, "⠙") || !strings.Contains(out, "publishing image (3/3 layers)") {
		t.Fatalf("animation or completion counts missing: %q", out)
	}
	if !strings.HasSuffix(out, "✓ Import \"demo\" complete (3/3 layers)\n") {
		t.Fatalf("final status = %q", out)
	}
}

func TestProgressStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &observedProgressWriter{changed: make(chan struct{}, 1)}
	p, err := newImageProgress(ctx, writer, `Pull "demo"`, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stopAnimation)
	cancel()
	waitProgressDone(t, p)
	if err := p.Finish(ctx.Err()); err != nil {
		t.Fatal(err)
	}
	out, _ := writer.snapshot()
	if !strings.HasSuffix(out, "✗ Pull \"demo\" canceled\n") {
		t.Fatalf("canceled status = %q", out)
	}
}

func TestProgressRetainsAnimationFailureAfterCommit(t *testing.T) {
	failure := errors.New("terminal write failed")
	writer := &observedProgressWriter{changed: make(chan struct{}, 1), failAt: 2, failure: failure}
	p, err := newImageProgress(t.Context(), writer, `Import "demo"`, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stopAnimation)
	waitProgressDone(t, p)
	if err := p.Status("converting layers"); !errors.Is(err, failure) {
		t.Fatalf("status error = %v", err)
	}
	if err := p.Committed(images.Image{}); !errors.Is(err, failure) {
		t.Fatalf("commit report error = %v", err)
	}
	err = p.Finish(failure)
	var classified *errdefs.Error
	if !errors.Is(err, failure) || !errors.As(err, &classified) || !classified.Committed {
		t.Fatalf("final report error = %v", err)
	}
	out, _ := writer.snapshot()
	if !strings.Contains(out, "committed with errors") {
		t.Fatalf("committed error status = %q", out)
	}
}

func TestProgressKeepsResultsSeparateFromLiveFrames(t *testing.T) {
	writer := &observedProgressWriter{changed: make(chan struct{}, 1)}
	p, err := newImageProgress(t.Context(), writer, `Verify "demo"`, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stopAnimation)
	if _, err := p.Output(writer).Write([]byte("verified sha256:example\n")); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(nil); err != nil {
		t.Fatal(err)
	}
	out, _ := writer.snapshot()
	if !strings.Contains(out, "\r\x1b[2Kverified sha256:example\n\r\x1b[2K") {
		t.Fatalf("result was not separated from the live spinner: %q", out)
	}
	waitProgressDone(t, p)
}

func TestRemovalProgressCountsCompletedImages(t *testing.T) {
	var out bytes.Buffer
	p, err := newImageProgress(t.Context(), &out, `Remove "demo, alias"`, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Removed(2); err != nil {
		t.Fatal(err)
	}
	if err := p.Removed(2); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "complete (2/2 images)\n") {
		t.Fatalf("removal status = %q", out.String())
	}
}
