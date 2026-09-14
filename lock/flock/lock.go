package flock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	goflock "github.com/gofrs/flock"
)

const retryInterval = 2 * time.Millisecond

// Lock combines in-process serialization with an advisory cross-process lock.
type Lock struct {
	path      string
	token     chan struct{}
	held      *goflock.Flock
	transient bool
}

func New(path string) *Lock {
	return &Lock{path: path, token: make(chan struct{}, 1)}
}

func NewTransient(path string) *Lock {
	return &Lock{path: path, token: make(chan struct{}, 1), transient: true}
}

func (l *Lock) Lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if info, err := os.Lstat(l.path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("lock path %s is not a regular file", l.path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	select {
	case l.token <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("wait for lock %s: %w", l.path, ctx.Err())
	}
	for {
		candidate := goflock.New(l.path)
		ok, err := candidate.TryLockContext(ctx, retryInterval)
		if err != nil || !ok {
			closeErr := candidate.Close()
			<-l.token
			if err == nil {
				err = ctx.Err()
			}
			return errors.Join(fmt.Errorf("acquire lock %s: %w", l.path, err), closeErr)
		}
		l.held = candidate
		if !l.transient || l.boundToPath() {
			return nil
		}
		if err := candidate.Close(); err != nil {
			<-l.token
			l.held = nil
			return fmt.Errorf("requeue stale lock %s: %w", l.path, err)
		}
		l.held = nil
	}
}

func (l *Lock) TryLock(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if info, err := os.Lstat(l.path); err == nil && !info.Mode().IsRegular() {
		return false, fmt.Errorf("lock path %s is not a regular file", l.path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	select {
	case l.token <- struct{}{}:
	default:
		return false, nil
	}
	candidate := goflock.New(l.path)
	ok, err := candidate.TryLock()
	if err != nil || !ok {
		closeErr := candidate.Close()
		<-l.token
		return false, errors.Join(err, closeErr)
	}
	l.held = candidate
	if l.transient && !l.boundToPath() {
		closeErr := candidate.Close()
		l.held = nil
		<-l.token
		return false, closeErr
	}
	return true, nil
}

func (l *Lock) Unlock(context.Context) error {
	if l.held == nil {
		return nil
	}
	var removeErr error
	if l.transient {
		removeErr = os.Remove(l.path)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
	}
	closeErr := l.held.Close()
	l.held = nil
	<-l.token
	if err := errors.Join(removeErr, closeErr); err != nil {
		return fmt.Errorf("release lock %s: %w", l.path, err)
	}
	return nil
}

func (l *Lock) boundToPath() bool {
	held, err := l.held.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(l.path)
	return err == nil && os.SameFile(held, current)
}
