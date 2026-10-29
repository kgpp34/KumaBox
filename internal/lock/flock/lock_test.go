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

func TestSharedReadersBlockExclusiveWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.lock")
	first, second, writer := New(path), New(path), New(path)
	if err := first.RLock(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := second.RLock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.Unlock(t.Context()); err != nil {
			t.Error(err)
		}
		if err := second.Unlock(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := writer.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writer while readers hold leases = %v, want deadline", err)
	}
	if err := first.Unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := writer.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writer while second reader holds lease = %v, want deadline", err)
	}
	if err := second.Unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := writer.Unlock(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := first.RLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader while writer holds lock = %v, want deadline", err)
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

func TestSharedSetsProtectEveryPathUntilReadersRelease(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "a.lock"), filepath.Join(t.TempDir(), "b.lock")
	var first, second Set
	if err := first.RLock(t.Context(), b, a, a); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Unlock(t.Context()) }()
	if err := second.RLock(t.Context(), a, b); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Unlock(t.Context()) }()
	if err := first.Unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{a, b} {
		writer := New(path)
		if ok, err := writer.TryLock(t.Context()); err != nil || ok {
			_ = writer.Unlock(t.Context())
			t.Fatalf("writer acquired %s while the second reader holds it: %v, %v", path, ok, err)
		}
	}
	if err := second.Unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
	var writer Set
	if err := writer.Lock(t.Context(), b, a); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Unlock(t.Context()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := first.RLock(ctx, a, b); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader while exclusive set holds paths: %v", err)
	}
}

func TestSharedSetCancellationReleasesPartialLease(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a.lock"), filepath.Join(base, "b.lock")
	blocker := New(b)
	if err := blocker.Lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Unlock(t.Context()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var reader Set
	if err := reader.RLock(ctx, b, a, a); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked shared set: %v", err)
	}
	probe := New(a)
	if ok, err := probe.TryLock(t.Context()); err != nil || !ok {
		t.Fatalf("partial read lease leaked: %v, %v", ok, err)
	}
	if err := probe.Unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
}
