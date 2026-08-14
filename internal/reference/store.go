// Package reference stores explicit ownership and dependency relationships
// between durable resources.
package reference

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
)

const (
	namespace meta.Namespace = "references"
	table     meta.Table     = "records"
)

type Record struct {
	ID         string    `json:"id"`
	SourceKind string    `json:"sourceKind"`
	SourceID   string    `json:"sourceId"`
	TargetKind string    `json:"targetKind"`
	TargetID   string    `json:"targetId"`
	Mode       string    `json:"mode"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Store struct {
	engine     meta.MetaEngine
	collection *meta.Collection[Record]
}

func New(rootDir string) *Store {
	engine, err := metajson.Open(JSONNamespace(rootDir))
	if err != nil {
		panic(fmt.Sprintf("open reference metadata engine: %v", err))
	}
	return NewWithEngine(engine)
}

// JSONNamespace describes references used by the JSON metadata backend.
func JSONNamespace(rootDir string) metajson.Namespace {
	return metajson.Namespace{
		Name: string(namespace), FilePath: filepath.Join(rootDir, "references", "records.json"), LockPath: filepath.Join(rootDir, "references", "records.lock"),
		Codec: metajson.TableCodec{Specs: []metajson.TableSpec{{Key: string(table), Table: string(table)}}},
	}
}

func NewWithEngine(engine meta.MetaEngine) *Store {
	return &Store{engine: engine, collection: meta.NewCollection[Record](namespace, table)}
}

func (s *Store) MetadataEngine() meta.MetaEngine { return s.engine }

func (s *Store) Upsert(ctx context.Context, record Record) error {
	if record.ID == "" || record.SourceKind == "" || record.SourceID == "" || record.TargetKind == "" || record.TargetID == "" {
		return fmt.Errorf("reference identity is incomplete: %w", meta.ErrScope)
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	return s.engine.Update(ctx, meta.Scope{Write: namespace}, meta.CommitDurable, func(writer meta.Writer) error {
		return s.collection.Upsert(ctx, writer, meta.RecordID(record.ID), &record)
	})
}

func (s *Store) Delete(ctx context.Context, id string) error {
	return s.engine.Update(ctx, meta.Scope{Write: namespace}, meta.CommitDurable, func(writer meta.Writer) error {
		return s.collection.Delete(ctx, writer, meta.RecordID(id))
	})
}

// DeleteSource removes every relationship owned by one durable resource in a
// single metadata transaction.
func (s *Store) DeleteSource(ctx context.Context, kind, id string) error {
	if kind == "" || id == "" {
		return fmt.Errorf("reference source identity is incomplete: %w", meta.ErrScope)
	}
	return s.engine.Update(ctx, meta.Scope{Write: namespace}, meta.CommitDurable, func(writer meta.Writer) error {
		var ids []meta.RecordID
		if err := s.collection.Scan(ctx, writer, func(recordID meta.RecordID, record *Record) error {
			if record.SourceKind == kind && record.SourceID == id {
				ids = append(ids, recordID)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, recordID := range ids {
			if err := s.collection.Delete(ctx, writer, recordID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ListTarget(ctx context.Context, kind, id string) ([]Record, error) {
	return s.list(ctx, func(record Record) bool { return record.TargetKind == kind && record.TargetID == id })
}

func (s *Store) ListSource(ctx context.Context, kind, id string) ([]Record, error) {
	return s.list(ctx, func(record Record) bool { return record.SourceKind == kind && record.SourceID == id })
}

func (s *Store) list(ctx context.Context, matches func(Record) bool) ([]Record, error) {
	var result []Record
	err := s.engine.View(ctx, []meta.Namespace{namespace}, func(reader meta.Reader) error {
		return s.collection.Scan(ctx, reader, func(_ meta.RecordID, record *Record) error {
			if matches(*record) {
				result = append(result, *record)
			}
			return nil
		})
	})
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, err
}

var _ interface {
	Upsert(context.Context, Record) error
	Delete(context.Context, string) error
	DeleteSource(context.Context, string, string) error
	ListTarget(context.Context, string, string) ([]Record, error)
	ListSource(context.Context, string, string) ([]Record, error)
} = (*Store)(nil)
