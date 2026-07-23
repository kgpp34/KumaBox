// Package meta defines the persistence boundary shared by metadata engines.
// It deliberately knows nothing about JSON files, SQLite tables, or host
// resources.
package meta

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrNotFound           = errors.New("metadata record not found")
	ErrConflict           = errors.New("metadata record conflict")
	ErrBusy               = errors.New("metadata store busy")
	ErrCorrupt            = errors.New("metadata store corrupt")
	ErrNoSpace            = errors.New("metadata store has no space")
	ErrIO                 = errors.New("metadata store I/O error")
	ErrScope              = errors.New("metadata scope violation")
	ErrClosed             = errors.New("metadata store is closed")
	ErrDurabilityContract = errors.New("durability contract violation")
)

// CommitMode controls the durability required from a successful update.
type CommitMode uint8

const (
	CommitDurable CommitMode = iota
	CommitRelaxed
)

// Scope declares the metadata namespaces an update may access. Write is the
// only namespace the transaction may modify; Read declares the other
// namespaces it may inspect. Engines acquire declared namespaces in a stable
// order so multi-namespace operations cannot deadlock.
type Scope struct {
	Write Namespace
	Read  []Namespace
}

// Namespace identifies one independently locked metadata document.
type Namespace string

// Table identifies a logical collection inside a namespace.
type Table string

// RecordID identifies one record inside a table.
type RecordID string

// MetaEngine is the engine-neutral metadata transaction boundary.
type MetaEngine interface {
	View(ctx context.Context, namespaces []Namespace, fn func(Reader) error) error
	Update(context.Context, Scope, CommitMode, func(Writer) error) error
	Events(context.Context) (<-chan struct{}, func(), error)
	Close() error
}

// Reader is the low-level storage SPI used by Collection. Resource code should
// normally use a typed Collection instead of handling encoded values directly.
type Reader interface {
	GetRaw(ctx context.Context, namespace Namespace, table Table, id RecordID) (json.RawMessage, bool, error)
	ScanRaw(ctx context.Context, namespace Namespace, table Table, fn func(RecordID, json.RawMessage) error) error
}

// Writer is the low-level write SPI. All mutations are discarded when the
// callback returns an error. Collection is the typed boundary above it.
type Writer interface {
	Reader
	PutRaw(ctx context.Context, namespace Namespace, table Table, id RecordID, raw json.RawMessage) error
	DeleteRaw(ctx context.Context, namespace Namespace, table Table, id RecordID) error
}
