package lock

import (
	"context"
	"fmt"
	"path/filepath"
)

const maintenanceKey = "maintenance"

// EntityKind identifies a lock domain for one durable resource type.
type EntityKind string

const (
	// EntityImage coordinates image references with image deletion.
	EntityImage EntityKind = "image"
)

// Guard owns the stable cross-process locks for one KumaBox root.
type Guard struct {
	locks *Locker
}

// NewGuard creates a guard rooted in KumaBox's durable lock directory.
func NewGuard(rootDir string) *Guard {
	return &Guard{locks: NewLocker(filepath.Join(rootDir, "locks", "resources"))}
}

// BeginMutation permits concurrent ordinary mutations while excluding GC.
func (g *Guard) BeginMutation(ctx context.Context) (*Lock, error) {
	lock, err := g.locks.AcquireShared(ctx, maintenanceKey)
	if err != nil {
		return nil, fmt.Errorf("lock resource mutation: %w", err)
	}
	return lock, nil
}

// BeginMaintenance excludes all guarded mutations for a complete GC cycle.
func (g *Guard) BeginMaintenance(ctx context.Context) (*Lock, error) {
	lock, err := g.locks.Acquire(ctx, maintenanceKey)
	if err != nil {
		return nil, fmt.Errorf("lock resource maintenance: %w", err)
	}
	return lock, nil
}

// LockEntity serializes publication, reference changes, and deletion for one
// durable entity. Callers must acquire the maintenance lock first.
func (g *Guard) LockEntity(ctx context.Context, kind EntityKind, id string) (*Lock, error) {
	if kind == "" || id == "" {
		return nil, fmt.Errorf("resource lock kind and id must not be empty")
	}
	lock, err := g.locks.Acquire(ctx, string(kind)+"-"+id)
	if err != nil {
		return nil, fmt.Errorf("lock %s %s: %w", kind, id, err)
	}
	return lock, nil
}
