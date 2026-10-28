package flock

import (
	"context"
	"errors"
	"slices"
)

// Set owns an ordered group of persistent file locks. Its zero value is usable.
// One owner must serialize acquisition and release; do not use it concurrently.
type Set struct {
	// held records acquisition order for reverse release and partial-failure cleanup.
	held []*Lock
}

// Lock sorts and deduplicates paths so cooperating operations acquire in the same
// order. On failure it releases every lock already held by the set. Callers should
// acquire their full path set in one call to preserve ordering across operations.
func (s *Set) Lock(ctx context.Context, paths ...string) error {
	return s.acquire(ctx, false, paths...)
}

// RLock holds shared leases in the same order as Lock. Independent readers may
// proceed together while artifact publication and removal retain exclusive use.
func (s *Set) RLock(ctx context.Context, paths ...string) error {
	return s.acquire(ctx, true, paths...)
}

func (s *Set) acquire(ctx context.Context, shared bool, paths ...string) error {
	ordered := slices.Compact(slices.Sorted(slices.Values(paths)))
	for _, path := range ordered {
		item := New(path)
		if err := item.acquire(ctx, shared); err != nil {
			return errors.Join(err, s.Unlock(ctx))
		}
		s.held = append(s.held, item)
	}
	return nil
}

// Unlock releases in reverse acquisition order, attempts every release, and clears
// the set even when a lock reports a cleanup error.
func (s *Set) Unlock(ctx context.Context) error {
	var errs []error
	for _, item := range slices.Backward(s.held) {
		errs = append(errs, item.Unlock(ctx))
	}
	s.held = nil
	return errors.Join(errs...)
}
