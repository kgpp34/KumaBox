package metadata

import (
	"context"
	"fmt"
)

// Store is the engine-neutral metadata transaction boundary.
type Store interface {
	View(context.Context, func(Reader) error) error
	Update(context.Context, func(Writer) error) error
	Close() error
}

// Reader reads detached records from a consistent snapshot.
type Reader interface {
	Get(context.Context, Collection, string) ([]byte, bool, error)
	Scan(context.Context, Collection, func(string, []byte) error) error
}

// Writer mutates records in one atomic transaction.
type Writer interface {
	Reader
	Put(context.Context, Collection, string, []byte) error
	Delete(context.Context, Collection, string) error
}

// Collection identifies one fixed module-owned record set.
type Collection string

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
func (c Collection) String() string { return string(c) }
