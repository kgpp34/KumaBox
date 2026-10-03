package flock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockSerializesInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entity.lock")
	first, second := New(path), New(path)
	if err := first.Lock(t.Context()); err != nil {
		t.Fatalf("first lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := second.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want deadline", err)
	}
	if err := first.Unlock(t.Context()); err != nil {
		t.Fatalf("unlock: %v", err)
	}
}

func TestTransientRemovesPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "init.lock")
	lock := NewTransient(path)
	if err := lock.Lock(t.Context()); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := lock.Unlock(t.Context()); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock path remains: %v", err)
	}
}

func TestSetReleasesPartialAcquisitionAndPersistentLocksKeepInode(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a.lock"), filepath.Join(base, "b.lock")
	blocker := New(b)
	if err := blocker.Lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocker.Unlock(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	var set Set
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := set.Lock(ctx, b, a, a); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("set error = %v", err)
	}
	probe := New(a)
	if ok, err := probe.TryLock(t.Context()); err != nil || !ok {
		t.Fatalf("partial lock leaked = %v, %v", ok, err)
	}
	before, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(a)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("persistent lock inode changed: %v", err)
	}
}
