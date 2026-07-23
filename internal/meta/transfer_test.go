package meta_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/meta"
	metajson "github.com/kumabox/kumabox/internal/meta/json"
	metasqlite "github.com/kumabox/kumabox/internal/meta/sqlite"
)

func TestTransferCopiesJSONMetadataIntoSQLite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	jsonEngine, err := metajson.Open(metajson.Namespace{
		Name:     "vms",
		FilePath: filepath.Join(dir, "vms.json"),
		LockPath: filepath.Join(dir, "vms.lock"),
		Codec:    metajson.TableCodec{Specs: []metajson.TableSpec{{Key: "records", Table: "records"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer jsonEngine.Close()

	type record struct {
		Name string `json:"name"`
	}
	collection := meta.NewCollection[record]("vms", "records")
	if err := jsonEngine.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(writer meta.Writer) error {
		return collection.Upsert(ctx, writer, "vm-1", &record{Name: "source"})
	}); err != nil {
		t.Fatal(err)
	}

	sqliteEngine, err := metasqlite.Open(filepath.Join(dir, "metadata.db"), metasqlite.Namespace{
		Name: "vms", Tables: []meta.Table{"records"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sqliteEngine.Close()

	if err := meta.Transfer(ctx, jsonEngine, sqliteEngine, []meta.TableSet{{
		Namespace: "vms",
		Tables:    []meta.Table{"records"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sqliteEngine.View(ctx, []meta.Namespace{"vms"}, func(reader meta.Reader) error {
		got, err := collection.Get(ctx, reader, "vm-1")
		if err != nil {
			return err
		}
		if got.Name != "source" {
			t.Fatalf("transferred record = %#v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
