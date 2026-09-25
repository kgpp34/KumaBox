// Package flock provides context-aware advisory file locks and ordered lock sets.
// Locks coordinate cooperating KumaBox operations; they are not security boundaries.
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

// retryInterval bounds polling delay between advisory lock acquisition attempts.
const retryInterval = 2 * time.Millisecond

// Lock combines in-process acquisition serialization with an advisory cross-process
// lock. Construct it with New or NewTransient; its zero value is not usable.
// Acquisitions may compete, but the owner must serialize Unlock and release once.
type Lock struct {
	// path identifies the lock file shared by cooperating processes.
	path string
	// token reserves this instance until acquisition fails or its owner unlocks.
	token chan struct{}
	// held is the owner's locked descriptor; only one acquisition can install it.
	held *goflock.Flock
	// transient removes the path before closing and rejects descriptors for old inodes.
	transient bool
}

// New creates a persistent lock whose file remains after release. Keeping its inode
// stable prevents waiters from locking an unlinked file while others use a new one.
func New(path string) *Lock {
	return &Lock{path: path, token: make(chan struct{}, 1)}
}

// NewTransient creates a removable coordination lock. Acquired descriptors are
// checked against the current path so waiters cannot proceed on an unlinked inode.
func NewTransient(path string) *Lock {
	return &Lock{path: path, token: make(chan struct{}, 1), transient: true}
}

// Lock waits for the local token and the advisory lock until cancellation.
// Transient locks requeue if a preceding owner removed their file while waiting.
//
//	local token -> file lock -> transient inode check -> owner
//	                   ^                  |
//	                   +--- stale inode --+
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

// TryLock performs one nonblocking attempt. Contention or a stale transient inode
// returns false without ownership; acquisition and descriptor cleanup errors survive.
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

// Unlock releases ownership even if the acquisition context has been canceled.
// For transient locks, removal precedes descriptor close so queued owners can detect
// a stale inode. Calling it without ownership is harmless, but concurrent calls are not.
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

// boundToPath verifies that the locked descriptor still names the current inode.
func (l *Lock) boundToPath() bool {
	held, err := l.held.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(l.path)
	return err == nil && os.SameFile(held, current)
}
