package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestRunVMBatchBestEffortPreservesOrderAndDeduplicates(t *testing.T) {
	wantErr := errors.New("start failed")
	var calls sync.Map

	result := runVMBatch(t.Context(), []string{"first", "failed", "first", "last"}, BatchOptions{Concurrency: 3},
		func(_ context.Context, ref string) (*vmstore.VMRecord, error) {
			count, _ := calls.LoadOrStore(ref, new(atomic.Int32))
			count.(*atomic.Int32).Add(1)
			if ref == "failed" {
				return nil, wantErr
			}
			return &vmstore.VMRecord{Name: ref}, nil
		})

	if len(result.Succeeded) != 2 || result.Succeeded[0].Name != "first" || result.Succeeded[1].Name != "last" {
		t.Fatalf("succeeded = %+v", result.Succeeded)
	}
	if len(result.Failed) != 1 || result.Failed[0].Ref != "failed" || result.Failed[0].Error != wantErr.Error() {
		t.Fatalf("failed = %+v", result.Failed)
	}
	if !errors.Is(result.Err(), wantErr) {
		t.Fatalf("error = %v, want wrapped %v", result.Err(), wantErr)
	}
	count, ok := calls.Load("first")
	if !ok || count.(*atomic.Int32).Load() != 1 {
		t.Fatalf("first call count = %v, want 1", count)
	}
}

func TestRunVMBatchHonorsConcurrencyLimit(t *testing.T) {
	var active atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 4)

	done := make(chan BatchResult, 1)
	go func() {
		done <- runVMBatch(t.Context(), []string{"a", "b", "c", "d"}, BatchOptions{Concurrency: 2},
			func(_ context.Context, ref string) (*vmstore.VMRecord, error) {
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
				return &vmstore.VMRecord{Name: ref}, nil
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

	result := <-done
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak.Load())
	}
}

func TestRunVMBatchReportsCanceledItems(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result := runVMBatch(ctx, []string{"a", "b"}, BatchOptions{Concurrency: 1},
		func(context.Context, string) (*vmstore.VMRecord, error) {
			t.Fatal("operation ran after context cancellation")
			return nil, nil
		})

	if len(result.Succeeded) != 0 || len(result.Failed) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if !errors.Is(result.Err(), context.Canceled) {
		t.Fatalf("error = %v, want context canceled", result.Err())
	}
}
