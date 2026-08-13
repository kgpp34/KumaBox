// Package batch runs bounded, best-effort operations over named resources.
package batch

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
)

// Options controls the amount of parallel work. Zero uses the host CPU count.
type Options struct {
	Concurrency int
}

// Failure describes one resource that could not complete an operation.
type Failure struct {
	Ref   string `json:"ref"`
	Error string `json:"error"`
}

// Result is the stable, input-ordered outcome of a best-effort batch.
type Result[T any] struct {
	Succeeded []T       `json:"succeeded"`
	Failed    []Failure `json:"failed,omitempty"`
	errors    []error
}

// Err joins all per-resource failures while retaining their error chains.
func (r Result[T]) Err() error {
	return errors.Join(r.errors...)
}

type item[T any] struct {
	ref   string
	value T
	err   error
}

// Run executes fn once for each ref with bounded concurrency.
func Run[T any](
	ctx context.Context,
	refs []string,
	opts Options,
	operation string,
	fn func(context.Context, int, string) (T, error),
) Result[T] {
	if len(refs) == 0 {
		return Result[T]{Succeeded: []T{}}
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}
	concurrency = min(concurrency, len(refs))

	items := make([]item[T], len(refs))
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			for index := range jobs {
				ref := refs[index]
				if err := ctx.Err(); err != nil {
					items[index] = item[T]{ref: ref, err: err}
					continue
				}
				value, err := fn(ctx, index, ref)
				items[index] = item[T]{ref: ref, value: value, err: err}
			}
		}()
	}
	for index := range refs {
		jobs <- index
	}
	close(jobs)
	workers.Wait()

	result := Result[T]{Succeeded: make([]T, 0, len(items))}
	for _, item := range items {
		if item.err == nil {
			result.Succeeded = append(result.Succeeded, item.value)
			continue
		}
		result.Failed = append(result.Failed, Failure{Ref: item.ref, Error: item.err.Error()})
		result.errors = append(result.errors, fmt.Errorf("%s %s: %w", operation, item.ref, item.err))
	}
	return result
}

// Distinct preserves the first occurrence of each resource reference.
func Distinct(refs []string) []string {
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
