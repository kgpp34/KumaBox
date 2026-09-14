package flock

import (
	"context"
	"errors"
	"slices"
)

// Set owns an ordered group of persistent file locks.
type Set struct {
	held []*Lock
}

func (s *Set) Lock(ctx context.Context, paths ...string) error {
	ordered := slices.Compact(slices.Sorted(slices.Values(paths)))
	for _, path := range ordered {
		item := New(path)
		if err := item.Lock(ctx); err != nil {
			return errors.Join(err, s.Unlock(ctx))
		}
		s.held = append(s.held, item)
	}
	return nil
}

func (s *Set) Unlock(ctx context.Context) error {
	var errs []error
	for _, item := range slices.Backward(s.held) {
		errs = append(errs, item.Unlock(ctx))
	}
	s.held = nil
	return errors.Join(errs...)
}
