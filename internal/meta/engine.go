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
	Write string
	Read  []string
}

// MetaEngine is the engine-neutral metadata transaction boundary.
type MetaEngine interface {
	View(context.Context, []string, func(Reader) error) error
	Update(context.Context, Scope, CommitMode, func(Writer) error) error
	Events(context.Context) (<-chan struct{}, func(), error)
	Close() error
}

// Reader exposes detached metadata values inside one consistent view.
type Reader interface {
	GetRaw(context.Context, string, string, string) (json.RawMessage, bool, error)
	ScanRaw(context.Context, string, string, func(string, json.RawMessage) error) error
}

// Writer is the write-capable transaction view. All mutations are discarded
// when the callback returns an error.
type Writer interface {
	Reader
	PutRaw(context.Context, string, string, string, json.RawMessage) error
	DeleteRaw(context.Context, string, string, string) error
}
