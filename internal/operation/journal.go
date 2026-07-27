// Package operation records durable control-plane operations that may need
// reconciliation after the host process exits unexpectedly.
package operation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

const (
	KindVMStart             = "vm.start"
	KindVMStop              = "vm.stop"
	KindVMDelete            = "vm.delete"
	KindVMPause             = "vm.pause"
	KindVMResume            = "vm.resume"
	KindVMHibernate         = "vm.hibernate"
	KindNetworkAttach       = "network.attach"
	KindNetworkCleanup      = "network.cleanup"
	KindNetworkResize       = "network.resize"
	KindDiskAttach          = "disk.attach"
	KindDiskDetach          = "disk.detach"
	KindFilesystemAttach    = "filesystem.attach"
	KindFilesystemDetach    = "filesystem.detach"
	KindPCIAttach           = "pci.attach"
	KindPCIDetach           = "pci.detach"
	KindSnapshotCreateRun   = "snapshot.create-running"
	KindSnapshotCloneNative = "snapshot.clone-native"
	KindSnapshotRestoreDisk = "snapshot.restore-portable"
	KindSnapshotRestoreVM   = "snapshot.restore-native"
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
	RelatedID  string     `json:"relatedId,omitempty"`
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

// NewID returns a process-independent operation identifier.
func NewID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate operation id: %w", err)
	}
	return "op_" + hex.EncodeToString(raw[:]), nil
}

func New(rootDir string) *Journal {
	return NewWithEngine(mustOpenEngine(rootDir))
}

func NewWithEngine(engine meta.MetaEngine) *Journal {
	return &Journal{engine: engine, collection: meta.NewCollection[Record](namespace, table)}
}

func (j *Journal) MetadataEngine() meta.MetaEngine { return j.engine }

func (j *Journal) Begin(ctx context.Context, id, kind, resourceID string) (*Record, error) {
	return j.begin(ctx, id, kind, resourceID, "")
}

func (j *Journal) BeginWithRelated(ctx context.Context, id, kind, resourceID, relatedID string) (*Record, error) {
	return j.begin(ctx, id, kind, resourceID, relatedID)
}

func (j *Journal) begin(ctx context.Context, id, kind, resourceID, relatedID string) (*Record, error) {
	if id == "" || kind == "" || resourceID == "" {
		return nil, fmt.Errorf("operation id, kind, and resource id are required: %w", meta.ErrScope)
	}
	now := time.Now().UTC()
	record := &Record{ID: id, Kind: kind, ResourceID: resourceID, RelatedID: relatedID, Status: StatusRunning, StartedAt: now, Attempt: 1}
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

// BindResource records the concrete resource created by an operation whose
// output identity was not known when the operation began.
func (j *Journal) BindResource(ctx context.Context, id, resourceID string) (*Record, error) {
	if id == "" || resourceID == "" {
		return nil, fmt.Errorf("operation id and resource id are required: %w", meta.ErrScope)
	}
	var result Record
	err := j.engine.Update(ctx, meta.Scope{Write: namespace}, meta.CommitDurable, func(writer meta.Writer) error {
		record, err := j.collection.Get(ctx, writer, meta.RecordID(id))
		if err != nil {
			return err
		}
		record.ResourceID = resourceID
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

// Reconcile lets the caller inspect each interrupted operation and decide how
// to repair it. A successful callback publishes succeeded; an error publishes
// failed with the callback error. The callback runs outside metadata writes so
// it may inspect host resources without holding a database transaction.
func (j *Journal) Reconcile(ctx context.Context, repair func(context.Context, Record) error) error {
	if repair == nil {
		return fmt.Errorf("operation repair callback must not be nil: %w", meta.ErrScope)
	}
	records, err := j.Recoverable(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := repair(ctx, record); err != nil {
			if _, markErr := j.Fail(ctx, record.ID, err.Error()); markErr != nil {
				return fmt.Errorf("record operation %s failure: %w", record.ID, markErr)
			}
			continue
		}
		if _, err := j.Complete(ctx, record.ID); err != nil {
			return fmt.Errorf("complete reconciled operation %s: %w", record.ID, err)
		}
	}
	return nil
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
