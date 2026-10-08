package progress

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type observedWriter struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	writes  int
	changed chan struct{}
	failAt  int
	failure error
}

func (w *observedWriter) Write(data []byte) (int, error) {
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
	return w.buffer.Write(data)
}

func (w *observedWriter) snapshot() (string, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String(), w.writes
}

type manualTicker struct {
	ticks   chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func newManualTicker() *manualTicker {
	return &manualTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
}

func (t *manualTicker) factory(time.Duration) (<-chan time.Time, func()) {
	return t.ticks, func() { t.once.Do(func() { close(t.stopped) }) }
}

func waitForWrites(t *testing.T, writer *observedWriter, count int) {
	t.Helper()
	for {
		if _, writes := writer.snapshot(); writes >= count {
			return
		}
		select {
		case <-writer.changed:
		case <-time.After(time.Second):
			t.Fatalf("writer did not reach %d writes", count)
		}
	}
}

func waitForDone(t *testing.T, renderer *Renderer) {
	t.Helper()
	select {
	case <-renderer.done:
	case <-time.After(time.Second):
		t.Fatal("progress animation did not stop")
	}
}

func TestPlainRendererDeduplicatesStatusesAndSeparatesOutput(t *testing.T) {
	var progressOut, resultOut bytes.Buffer
	renderer, err := New(t.Context(), &progressOut, "Import · preparing")
	if err != nil {
		t.Fatal(err)
	}
	if renderer.Animated() {
		t.Fatal("buffer-backed renderer enabled animation")
	}
	for _, status := range []string{"Import · preparing", "Import · converting", "Import · converting"} {
		if err := renderer.Update(status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := renderer.Output(&resultOut).Write([]byte("sha256:example\n")); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Finish("Import", Succeeded, " (2/2 layers)"); err != nil {
		t.Fatal(err)
	}
	want := "Import · preparing\nImport · converting\nImport complete (2/2 layers)\n"
	if progressOut.String() != want || strings.ContainsAny(progressOut.String(), "\r\x1b") {
		t.Fatalf("plain progress = %q, want %q", progressOut.String(), want)
	}
	if resultOut.String() != "sha256:example\n" {
		t.Fatalf("result output = %q", resultOut.String())
	}
}

func TestAnimatedRendererUsesInjectedTicksAndJoins(t *testing.T) {
	writer := &observedWriter{changed: make(chan struct{}, 1)}
	ticker := newManualTicker()
	renderer, err := newRenderer(t.Context(), writer, "Start · preparing", true, ticker.factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(renderer.stopAnimation)
	ticker.ticks <- time.Time{}
	waitForWrites(t, writer, 2)
	if err := renderer.Update("Start · launching"); err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	if _, err := renderer.Output(&result).Write([]byte("sandbox-id\n")); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Finish("Start", Succeeded, ""); err != nil {
		t.Fatal(err)
	}
	waitForDone(t, renderer)
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("animation ticker was not stopped")
	}
	out, _ := writer.snapshot()
	if !strings.Contains(out, "⠋ Start · preparing") || !strings.Contains(out, "⠙ Start · preparing") {
		t.Fatalf("spinner did not advance from injected tick: %q", out)
	}
	if !strings.HasSuffix(out, "\r\x1b[2K✓ Start complete\n") {
		t.Fatalf("final terminal line = %q", out)
	}
	if result.String() != "sandbox-id\n" {
		t.Fatalf("result output = %q", result.String())
	}
}

func TestAnimatedRendererStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	writer := &observedWriter{changed: make(chan struct{}, 1)}
	ticker := newManualTicker()
	renderer, err := newRenderer(ctx, writer, "Pull · preparing", true, ticker.factory)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	waitForDone(t, renderer)
	if err := renderer.Finish("Pull", Canceled, ""); err != nil {
		t.Fatal(err)
	}
	out, _ := writer.snapshot()
	if !strings.HasSuffix(out, "\r\x1b[2K✗ Pull canceled\n") {
		t.Fatalf("canceled status = %q", out)
	}
}

func TestAnimatedRendererRetainsWriteFailure(t *testing.T) {
	failure := errors.New("terminal write failed")
	writer := &observedWriter{changed: make(chan struct{}, 1), failAt: 2, failure: failure}
	ticker := newManualTicker()
	renderer, err := newRenderer(t.Context(), writer, "Verify · preparing", true, ticker.factory)
	if err != nil {
		t.Fatal(err)
	}
	ticker.ticks <- time.Time{}
	waitForDone(t, renderer)
	if !errors.Is(renderer.Err(), failure) {
		t.Fatalf("retained error = %v", renderer.Err())
	}
	if err := renderer.Update("Verify · checking"); !errors.Is(err, failure) {
		t.Fatalf("update error = %v", err)
	}
	if err := renderer.Finish("Verify", Failed, ""); !errors.Is(err, failure) {
		t.Fatalf("finish error = %v", err)
	}
}

func TestInitialWriteFailureDoesNotStartTicker(t *testing.T) {
	failure := errors.New("initial write failed")
	writer := &observedWriter{failAt: 1, failure: failure}
	tickerStarted := false
	_, err := newRenderer(t.Context(), writer, "Create · preparing", true, func(time.Duration) (<-chan time.Time, func()) {
		tickerStarted = true
		return make(chan time.Time), func() {}
	})
	if !errors.Is(err, failure) {
		t.Fatalf("constructor error = %v", err)
	}
	if tickerStarted {
		t.Fatal("ticker started after initial rendering failed")
	}
}
