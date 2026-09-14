package metadata

import "context"

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
