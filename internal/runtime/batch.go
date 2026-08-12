package runtime

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// BatchOptions controls the amount of parallel lifecycle work. A zero
// concurrency uses the host CPU count.
type BatchOptions struct {
	Concurrency int
}

// BatchFailure describes one VM that could not complete a batch operation.
type BatchFailure struct {
	Ref   string `json:"ref"`
	Error string `json:"error"`
}

// BatchResult is the stable, input-ordered outcome of a best-effort batch.
type BatchResult struct {
	Succeeded []*vmstore.VMRecord `json:"succeeded"`
	Failed    []BatchFailure      `json:"failed,omitempty"`
	errors    []error
}

// Err joins all per-VM failures while retaining their original error chains.
func (r BatchResult) Err() error {
	return errors.Join(r.errors...)
}

// StartVMsContext starts each distinct VM reference using bounded concurrency.
func (r *Runtime) StartVMsContext(ctx context.Context, refs []string, opts BatchOptions) BatchResult {
	return runVMBatch(ctx, refs, opts, r.StartVMContext)
}

// StopVMsContext stops each distinct VM reference using bounded concurrency.
func (r *Runtime) StopVMsContext(
	ctx context.Context,
	refs []string,
	stopOpts backend.StopOptions,
	batchOpts BatchOptions,
) BatchResult {
	return runVMBatch(ctx, refs, batchOpts, func(ctx context.Context, ref string) (*vmstore.VMRecord, error) {
		return r.StopVMContext(ctx, ref, stopOpts)
	})
}

// PauseVMs pauses each distinct VM reference using bounded concurrency.
func (r *Runtime) PauseVMs(ctx context.Context, refs []string, opts BatchOptions) BatchResult {
	return runVMBatch(ctx, refs, opts, r.PauseVM)
}

// ResumeVMs resumes each distinct VM reference using bounded concurrency.
func (r *Runtime) ResumeVMs(ctx context.Context, refs []string, opts BatchOptions) BatchResult {
	return runVMBatch(ctx, refs, opts, r.ResumeVM)
}

// DeleteVMsContext deletes each distinct VM reference using bounded concurrency.
func (r *Runtime) DeleteVMsContext(ctx context.Context, refs []string, force bool, opts BatchOptions) BatchResult {
	return runVMBatch(ctx, refs, opts, func(ctx context.Context, ref string) (*vmstore.VMRecord, error) {
		return r.DeleteVMContext(ctx, ref, force)
	})
}

type vmBatchItem struct {
	ref    string
	record *vmstore.VMRecord
	err    error
}

func runVMBatch(
	ctx context.Context,
	refs []string,
	opts BatchOptions,
	fn func(context.Context, string) (*vmstore.VMRecord, error),
) BatchResult {
	refs = distinctVMRefs(refs)
	if len(refs) == 0 {
		return BatchResult{Succeeded: []*vmstore.VMRecord{}}
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}
	concurrency = min(concurrency, len(refs))

	items := make([]vmBatchItem, len(refs))
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			for index := range jobs {
				ref := refs[index]
				if err := ctx.Err(); err != nil {
					items[index] = vmBatchItem{ref: ref, err: err}
					continue
				}
				record, err := fn(ctx, ref)
				items[index] = vmBatchItem{ref: ref, record: record, err: err}
			}
		}()
	}
	for index := range refs {
		jobs <- index
	}
	close(jobs)
	workers.Wait()

	result := BatchResult{
		Succeeded: make([]*vmstore.VMRecord, 0, len(items)),
		Failed:    make([]BatchFailure, 0),
		errors:    make([]error, 0),
	}
	for _, item := range items {
		if item.err == nil {
			result.Succeeded = append(result.Succeeded, item.record)
			continue
		}
		result.Failed = append(result.Failed, BatchFailure{Ref: item.ref, Error: item.err.Error()})
		result.errors = append(result.errors, fmt.Errorf("VM %s: %w", item.ref, item.err))
	}
	return result
}

func distinctVMRefs(refs []string) []string {
	distinct := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if _, exists := seen[ref]; exists {
			continue
		}
		seen[ref] = struct{}{}
		distinct = append(distinct, ref)
	}
	return distinct
}
