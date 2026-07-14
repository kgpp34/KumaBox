package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const leaseRetryInterval = 25 * time.Millisecond

type leaseMode int

const (
	leaseRead leaseMode = iota
	leaseExclusive
)

// Lease owns one kernel flock until Release is called or the process exits.
type Lease struct {
	file *os.File
}

// Release unlocks the snapshot lease. It is safe to call more than once.
func (l *Lease) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

type leaser struct {
	dir string
}

func newLeaser(dir string) *leaser {
	return &leaser{dir: dir}
}

func (l *leaser) acquire(ctx context.Context, id string, mode leaseMode, wait bool) (*Lease, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot lease directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(l.dir, id+".lease"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open snapshot lease: %w", err)
	}
	operation := syscall.LOCK_SH | syscall.LOCK_NB
	if mode == leaseExclusive {
		operation = syscall.LOCK_EX | syscall.LOCK_NB
	}
	for {
		if err := syscall.Flock(int(file.Fd()), operation); err == nil {
			return &Lease{file: file}, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock snapshot lease: %w", err)
		}
		if !wait {
			_ = file.Close()
			return nil, ErrInUse
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("wait for snapshot lease: %w", ctx.Err())
		case <-time.After(leaseRetryInterval):
		}
	}
}
