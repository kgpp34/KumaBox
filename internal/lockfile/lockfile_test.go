package lockfile

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAcquireSerializesSameKey(t *testing.T) {
	locker := New(t.TempDir())
	first, err := locker.Acquire(context.Background(), "kb_same")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err = locker.Acquire(ctx, "kb_same")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want context deadline", err)
	}
}

func TestAcquireDoesNotSerializeDifferentKeys(t *testing.T) {
	locker := New(t.TempDir())
	first, err := locker.Acquire(context.Background(), "kb_first")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release() //nolint:errcheck

	second, err := locker.Acquire(context.Background(), "kb_second")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestSharedLocksRunConcurrentlyAndExcludeWriter(t *testing.T) {
	locker := New(t.TempDir())
	first, err := locker.AcquireShared(context.Background(), "resources")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release() //nolint:errcheck

	second, err := locker.AcquireShared(context.Background(), "resources")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err = locker.Acquire(ctx, "resources")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exclusive Acquire() error = %v, want context deadline", err)
	}
}

func TestExclusiveLockExcludesSharedReader(t *testing.T) {
	locker := New(t.TempDir())
	writer, err := locker.Acquire(context.Background(), "resources")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err = locker.AcquireShared(ctx, "resources")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireShared() error = %v, want context deadline", err)
	}
}

func TestReleaseAllowsReacquire(t *testing.T) {
	locker := New(t.TempDir())
	lock, err := locker.Acquire(context.Background(), "kb_release")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	next, err := locker.Acquire(context.Background(), "kb_release")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release() //nolint:errcheck
}
