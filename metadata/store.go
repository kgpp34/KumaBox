// Package metadata defines collection-scoped transactions without prescribing
// a storage engine or interpreting module-owned record payloads.
package metadata

import (
	"context"
	"fmt"
)

// Store is the engine-neutral metadata transaction boundary. Transaction handles
// are callback-scoped and must not escape or be used concurrently. Callers declare
// collections when constructing an engine; undeclared collections are rejected.
type Store interface {
	// View invokes its callback against one consistent read snapshot.
	// Callback failure or cancellation is returned to the caller.
	View(context.Context, func(Reader) error) error
	// Update commits all callback writes atomically on success and discards them
	// on failure. A callback is not retried after it starts.
	Update(context.Context, func(Writer) error) error
	// Close releases engine resources; subsequent transactions must fail.
	Close() error
}

// Reader reads detached records from a consistent snapshot.
type Reader interface {
	// Get returns caller-owned bytes and an existence flag; absence is not an error.
	Get(context.Context, Collection, string) ([]byte, bool, error)
	// Scan visits records in key order with detached bytes and stops on callback error.
	Scan(context.Context, Collection, func(string, []byte) error) error
}

// Writer mutates records in one atomic transaction.
type Writer interface {
	// Reader sees earlier writes in the same transaction.
	Reader
	// Put replaces a record, copying its bytes so later caller mutation is harmless.
	Put(context.Context, Collection, string, []byte) error
	// Delete removes a record; deleting an absent key succeeds.
	Delete(context.Context, Collection, string) error
}

// Collection identifies one fixed module-owned record set.
type Collection string

// NewCollection validates a fixed collection name: 1-63 lowercase ASCII letters,
// digits, or underscores, beginning with a letter. Records remain engine-neutral.
func NewCollection(name string) (Collection, error) {
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return "", fmt.Errorf("invalid metadata collection %q", name)
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return "", fmt.Errorf("invalid metadata collection %q", name)
		}
	}
	return Collection(name), nil
}

// String returns the collection name used by engine adapters.
func (c Collection) String() string { return string(c) }
