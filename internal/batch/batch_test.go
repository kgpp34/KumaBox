package batch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunPreservesOrderAndPartialSuccess(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("operation failed")
	result := Run(t.Context(), []string{"first", "failed", "last"}, Options{Concurrency: 2}, "test",
		func(_ context.Context, _ int, ref string) (string, error) {
			if ref == "failed" {
				return "", wantErr
			}
			return ref + "-done", nil
		})
	if len(result.Succeeded) != 2 || result.Succeeded[0] != "first-done" || result.Succeeded[1] != "last-done" {
		t.Fatalf("succeeded = %v", result.Succeeded)
	}
	if len(result.Failed) != 1 || result.Failed[0].Ref != "failed" {
		t.Fatalf("failed = %v", result.Failed)
	}
	if !errors.Is(result.Err(), wantErr) {
		t.Fatalf("error = %v, want wrapped %v", result.Err(), wantErr)
	}
}

func TestRunHonorsConcurrency(t *testing.T) {
	t.Parallel()
	var active atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	done := make(chan Result[string], 1)
	go func() {
		done <- Run(t.Context(), []string{"a", "b", "c", "d"}, Options{Concurrency: 2}, "test",
			func(_ context.Context, _ int, ref string) (string, error) {
				current := active.Add(1)
				for {
					previous := peak.Load()
					if current <= previous || peak.CompareAndSwap(previous, current) {
						break
					}
				}
				started <- struct{}{}
				<-release
				active.Add(-1)
				return ref, nil
			})
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("batch did not start two workers")
		}
	}
	select {
	case <-started:
		t.Fatal("batch exceeded concurrency limit")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := (<-done).Err(); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak.Load())
	}
}

func TestRunReportsCanceledItems(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := Run(ctx, []string{"a", "b"}, Options{Concurrency: 1}, "test",
		func(context.Context, int, string) (string, error) {
			t.Fatal("operation ran after context cancellation")
			return "", nil
		})
	if len(result.Succeeded) != 0 || len(result.Failed) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if !errors.Is(result.Err(), context.Canceled) {
		t.Fatalf("error = %v, want context canceled", result.Err())
	}
}

func TestDistinctPreservesFirstOccurrence(t *testing.T) {
	t.Parallel()
	got := Distinct([]string{"a", "b", "a", "c", "b"})
	want := []string{"a", "b", "c"}
	for i := range want {
		if len(got) != len(want) || got[i] != want[i] {
			t.Fatalf("distinct refs = %v, want %v", got, want)
		}
	}
}
