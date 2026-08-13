package runtime

import (
	"context"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/batch"
	"github.com/kumabox/kumabox/internal/vmstore"
)

// BatchOptions controls the amount of parallel lifecycle work. A zero
// concurrency uses the host CPU count.
type BatchOptions struct {
	Concurrency int
}

// BatchFailure describes one VM that could not complete a batch operation.
type BatchFailure = batch.Failure

// BatchResult is the stable, input-ordered outcome of a best-effort batch.
type BatchResult struct {
	Succeeded []*vmstore.VMRecord `json:"succeeded"`
	Failed    []BatchFailure      `json:"failed,omitempty"`
	err       error
}

// Err joins all per-VM failures while retaining their original error chains.
func (r BatchResult) Err() error {
	return r.err
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

func runVMBatch(
	ctx context.Context,
	refs []string,
	opts BatchOptions,
	fn func(context.Context, string) (*vmstore.VMRecord, error),
) BatchResult {
	refs = batch.Distinct(refs)
	if len(refs) == 0 {
		return BatchResult{Succeeded: []*vmstore.VMRecord{}}
	}
	result := batch.Run(ctx, refs, batch.Options{Concurrency: opts.Concurrency}, "VM",
		func(ctx context.Context, _ int, ref string) (*vmstore.VMRecord, error) {
			return fn(ctx, ref)
		})
	return BatchResult{Succeeded: result.Succeeded, Failed: result.Failed, err: result.Err()}
}
