// Package lockfile provides context-aware advisory locks for daemonless
// cross-process operations.
package lockfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const retryInterval = 25 * time.Millisecond

// Locker owns a directory of stable lock files.
type Locker struct {
	dir string
}

// Lock is an acquired advisory file lock.
type Lock struct {
	file *os.File
	once sync.Once
}

// New returns a Locker rooted at dir.
func New(dir string) *Locker {
	return &Locker{dir: dir}
}

// Acquire waits until key is exclusively locked or ctx is cancelled.
func (l *Locker) Acquire(ctx context.Context, key string) (*Lock, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", key, err)
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(l.dir, key+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", key, err)
	}

	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock %s: %w", key, err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("acquire lock %s: %w", key, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Release unlocks and closes the lock. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	var releaseErr error
	l.once.Do(func() {
		unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		closeErr := l.file.Close()
		releaseErr = errors.Join(unlockErr, closeErr)
	})
	if releaseErr != nil {
		return fmt.Errorf("release lock: %w", releaseErr)
	}
	return nil
}

func validateKey(key string) error {
	if key == "" || key == "." || key == ".." || strings.ContainsAny(key, `/\\`) {
		return fmt.Errorf("invalid lock key %q", key)
	}
	return nil
}
