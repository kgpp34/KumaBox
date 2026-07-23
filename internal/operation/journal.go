// Package operation records durable control-plane operations that may need
// reconciliation after the host process exits unexpectedly.
package operation

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
)

const (
	namespace meta.Namespace = "operations"
	table     meta.Table     = "records"
)

type Status string

const (
	StatusRunning Status = "running"
	StatusSuccess Status = "succeeded"
	StatusFailed  Status = "failed"
)

// Record is the durable intent and result of one control-plane operation.
type Record struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	ResourceID string     `json:"resourceId"`
	Status     Status     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
	Attempt    int        `json:"attempt"`
}

type Journal struct {
	engine     meta.MetaEngine
	collection *meta.Collection[Record]
}

func New(rootDir string) *Journal {
	return NewWithEngine(mustOpenEngine(rootDir))
}

func NewWithEngine(engine meta.MetaEngine) *Journal {
	return &Journal{engine: engine, collection: meta.NewCollection[Record](namespace, table)}
}

func (j *Journal) Begin(ctx context.Context, id, kind, resourceID string) (*Record, error) {
	if id == "" || kind == "" || resourceID == "" {
		return nil, fmt.Errorf("operation id, kind, and resource id are required: %w", meta.ErrScope)
	}
	now := time.Now().UTC()
	record := &Record{ID: id, Kind: kind, ResourceID: resourceID, Status: StatusRunning, StartedAt: now, Attempt: 1}
	err := j.engine.Update(ctx, meta.Scope{Write: namespace}, meta.CommitDurable, func(writer meta.Writer) error {
		previous, err := j.collection.Get(ctx, writer, meta.RecordID(id))
		if err == nil {
			record.Attempt = previous.Attempt + 1
		} else if !errors.Is(err, meta.ErrNotFound) {
			return err
		}
		return j.collection.Upsert(ctx, writer, meta.RecordID(id), record)
	})
	if err != nil {
		return nil, err
	}
	return clone(*record), nil
}

func (j *Journal) Complete(ctx context.Context, id string) (*Record, error) {
	return j.finish(ctx, id, StatusSuccess, "")
}

func (j *Journal) Fail(ctx context.Context, id, reason string) (*Record, error) {
	if reason == "" {
		return nil, fmt.Errorf("operation failure reason is required: %w", meta.ErrScope)
	}
	return j.finish(ctx, id, StatusFailed, reason)
}

func (j *Journal) finish(ctx context.Context, id string, status Status, reason string) (*Record, error) {
	var result Record
	err := j.engine.Update(ctx, meta.Scope{Write: namespace}, meta.CommitDurable, func(writer meta.Writer) error {
		record, err := j.collection.Get(ctx, writer, meta.RecordID(id))
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		record.Status = status
		record.FinishedAt = &now
		record.Error = reason
		if err := j.collection.Replace(ctx, writer, meta.RecordID(id), record); err != nil {
			return err
		}
		result = *record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return clone(result), nil
}

// Recoverable returns operations left running by a process that did not
// publish a terminal result. Reconciliation decides whether to retry or fail
// each operation; the journal does not guess at backend state.
func (j *Journal) Recoverable(ctx context.Context) ([]Record, error) {
	var records []Record
	err := j.engine.View(ctx, []meta.Namespace{namespace}, func(reader meta.Reader) error {
		return j.collection.Scan(ctx, reader, func(_ meta.RecordID, record *Record) error {
			if record.Status == StatusRunning {
				records = append(records, *record)
			}
			return nil
		})
	})
	return records, err
}

func clone(record Record) *Record {
	if record.FinishedAt != nil {
		finished := *record.FinishedAt
		record.FinishedAt = &finished
	}
	return &record
}

func mustOpenEngine(rootDir string) meta.MetaEngine {
	engine, err := metajson.Open(metajson.Namespace{
		Name:     string(namespace),
		FilePath: filepath.Join(rootDir, "operation", "records.json"),
		LockPath: filepath.Join(rootDir, "operation", "records.lock"),
		Codec:    metajson.TableCodec{Specs: []metajson.TableSpec{{Key: string(table), Table: string(table)}}},
	})
	if err != nil {
		panic(fmt.Sprintf("open operation metadata engine: %v", err))
	}
	return engine
}
