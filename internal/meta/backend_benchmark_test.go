package meta_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
)

type benchmarkRecord struct {
	Value int `json:"value"`
}

func BenchmarkMetadataUpdateJSON(b *testing.B) {
	benchmarkMetadataUpdate(b, func(dir string) (meta.MetaEngine, error) {
		return metajson.Open(metajson.Namespace{
			Name: "bench", FilePath: filepath.Join(dir, "records.json"), LockPath: filepath.Join(dir, "records.lock"),
			Codec: metajson.TableCodec{Specs: []metajson.TableSpec{{Key: "records", Table: "records"}}},
		})
	})
}

func BenchmarkMetadataUpdateSQLite(b *testing.B) {
	benchmarkMetadataUpdate(b, func(dir string) (meta.MetaEngine, error) {
		return metasqlite.Open(filepath.Join(dir, "metadata.db"), metasqlite.Namespace{Name: "bench", Tables: []meta.Table{"records"}})
	})
}

func benchmarkMetadataUpdate(b *testing.B, open func(string) (meta.MetaEngine, error)) {
	b.Helper()
	engine, err := open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer engine.Close()
	collection := meta.NewCollection[benchmarkRecord]("bench", "records")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		record := benchmarkRecord{Value: i}
		if err := engine.Update(ctx, meta.Scope{Write: "bench"}, meta.CommitRelaxed, func(writer meta.Writer) error {
			return collection.Upsert(ctx, writer, meta.RecordID("record"), &record)
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMetadataBackendsRollbackTheWholeUpdate(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(string) (meta.MetaEngine, error)
	}{
		{name: "json", open: func(dir string) (meta.MetaEngine, error) {
			return metajson.Open(metajson.Namespace{
				Name: "fault", FilePath: filepath.Join(dir, "records.json"), LockPath: filepath.Join(dir, "records.lock"),
				Codec: metajson.TableCodec{Specs: []metajson.TableSpec{{Key: "records", Table: "records"}}},
			})
		}},
		{name: "sqlite", open: func(dir string) (meta.MetaEngine, error) {
			return metasqlite.Open(filepath.Join(dir, "metadata.db"), metasqlite.Namespace{Name: "fault", Tables: []meta.Table{"records"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, err := tc.open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			collection := meta.NewCollection[benchmarkRecord]("fault", "records")
			ctx := context.Background()
			wantErr := errors.New("injected failure")
			if err := engine.Update(ctx, meta.Scope{Write: "fault"}, meta.CommitDurable, func(writer meta.Writer) error {
				record := benchmarkRecord{Value: 1}
				if err := collection.Upsert(ctx, writer, meta.RecordID("record"), &record); err != nil {
					return err
				}
				return wantErr
			}); !errors.Is(err, wantErr) {
				t.Fatalf("update error = %v", err)
			}
			if err := engine.View(ctx, []meta.Namespace{"fault"}, func(reader meta.Reader) error {
				_, err := collection.Get(ctx, reader, meta.RecordID("record"))
				if !errors.Is(err, meta.ErrNotFound) {
					return errors.New("failed update was persisted")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
