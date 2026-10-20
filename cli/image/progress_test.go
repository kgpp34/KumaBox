package image

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

func TestProgressLogsHaveNoAnimationControls(t *testing.T) {
	var out bytes.Buffer
	progress, err := newImageProgress(t.Context(), &out, `Import "demo"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := progress.Status("converting layers"); err != nil {
		t.Fatal(err)
	}
	if err := progress.Layer(1, 2, types.Digest{}); err != nil {
		t.Fatal(err)
	}
	if err := progress.Layer(0, 2, types.Digest{}); err != nil {
		t.Fatal(err)
	}
	if err := progress.Committed(types.Image{}); err != nil {
		t.Fatal(err)
	}
	if err := progress.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\r\x1b") || !strings.Contains(out.String(), "complete (2/2 layers)\n") {
		t.Fatalf("plain progress = %q", out.String())
	}
	if !strings.Contains(out.String(), "preparing image") || strings.Count(out.String(), " complete\n") != 2 {
		t.Fatalf("missing initial status or layer notifications: %s", out.String())
	}
}

func TestProgressCountsConcurrentCompletedLayers(t *testing.T) {
	var out bytes.Buffer
	progress, err := newImageProgress(t.Context(), &out, `Import "demo"`)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for _, position := range []int{2, 0, 1} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := progress.Layer(position, 3, types.Digest{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	if err := progress.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "Layer ") != 3 || !strings.HasSuffix(out.String(), "complete (3/3 layers)\n") {
		t.Fatalf("layer progress = %q", out.String())
	}
}

type imageFailWriter struct {
	bytes.Buffer
	writes  int
	failAt  int
	failure error
}

func (w *imageFailWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.failure
	}
	return w.Buffer.Write(data)
}

func TestProgressRetainsRenderingFailureAfterCommit(t *testing.T) {
	failure := errors.New("terminal write failed")
	writer := &imageFailWriter{failAt: 2, failure: failure}
	progress, err := newImageProgress(t.Context(), writer, `Import "demo"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := progress.Status("converting layers"); !errors.Is(err, failure) {
		t.Fatalf("status error = %v", err)
	}
	if err := progress.Committed(types.Image{}); !errors.Is(err, failure) {
		t.Fatalf("commit report error = %v", err)
	}
	err = progress.Finish(failure)
	var classified *errdefs.Error
	if !errors.Is(err, failure) || !errors.As(err, &classified) || !classified.Committed {
		t.Fatalf("final report error = %v", err)
	}
	if !strings.Contains(writer.String(), "committed with errors") {
		t.Fatalf("committed error status = %q", writer.String())
	}
}

func TestProgressKeepsResultsOnTheirWriter(t *testing.T) {
	var progressOut, resultOut bytes.Buffer
	progress, err := newImageProgress(t.Context(), &progressOut, `Verify "demo"`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := progress.Output(&resultOut).Write([]byte("verified sha256:example\n")); err != nil {
		t.Fatal(err)
	}
	if err := progress.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(progressOut.String(), "sha256:example") || resultOut.String() != "verified sha256:example\n" {
		t.Fatalf("progress=%q result=%q", progressOut.String(), resultOut.String())
	}
}

func TestProgressReportsCancellation(t *testing.T) {
	var out bytes.Buffer
	progress, err := newImageProgress(t.Context(), &out, `Pull "demo"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := progress.Finish(context.Canceled); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), `Pull "demo" canceled`+"\n") {
		t.Fatalf("canceled status = %q", out.String())
	}
}

func TestRemovalProgressCountsCompletedImages(t *testing.T) {
	var out bytes.Buffer
	progress, err := newImageProgress(t.Context(), &out, `Remove "demo, alias"`)
	if err != nil {
		t.Fatal(err)
	}
	if err := progress.Removed(2); err != nil {
		t.Fatal(err)
	}
	if err := progress.Removed(2); err != nil {
		t.Fatal(err)
	}
	if err := progress.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "complete (2/2 images)\n") {
		t.Fatalf("removal status = %q", out.String())
	}
}
