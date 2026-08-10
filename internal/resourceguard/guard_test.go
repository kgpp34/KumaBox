package resourceguard

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMaintenanceWaitsForEveryMutation(t *testing.T) {
	guard := New(t.TempDir())
	first, err := guard.BeginMutation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := guard.BeginMutation(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Millisecond)
	defer cancel()
	_, err = guard.BeginMaintenance(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BeginMaintenance() error = %v, want context deadline", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}

	maintenance, err := guard.BeginMaintenance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestEntityLocksAreScopedByKindAndID(t *testing.T) {
	guard := New(t.TempDir())
	image, err := guard.LockEntity(t.Context(), EntityImage, "img_one")
	if err != nil {
		t.Fatal(err)
	}
	defer image.Release() //nolint:errcheck

	other, err := guard.LockEntity(t.Context(), EntityImage, "img_two")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Millisecond)
	defer cancel()
	_, err = guard.LockEntity(ctx, EntityImage, "img_one")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("LockEntity() error = %v, want context deadline", err)
	}
}
