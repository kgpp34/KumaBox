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
	defer func() {
		if err := jsonEngine.Close(); err != nil {
			t.Errorf("close JSON engine: %v", err)
		}
	}()

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
	defer func() {
		if err := sqliteEngine.Close(); err != nil {
			t.Errorf("close SQLite engine: %v", err)
		}
	}()

	report, err := meta.TransferWithReport(ctx, jsonEngine, sqliteEngine, []meta.TableSet{{
		Namespace: "vms",
		Tables:    []meta.Table{"records"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Records["vms"] != 1 || report.Digest == "" {
		t.Fatalf("transfer report = %+v", report)
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

func TestSQLiteConversionMarksNamespacesAfterTransfer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source, err := metajson.Open(metajson.Namespace{
		Name: "vms", FilePath: filepath.Join(dir, "vms.json"), LockPath: filepath.Join(dir, "vms.lock"),
		Codec: metajson.TableCodec{Specs: []metajson.TableSpec{{Key: "records", Table: "records"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			t.Errorf("close source engine: %v", err)
		}
	}()
	collection := meta.NewCollection[map[string]string]("vms", "records")
	record := map[string]string{"name": "source"}
	if err := source.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(writer meta.Writer) error {
		return collection.Upsert(ctx, writer, "vm-1", &record)
	}); err != nil {
		t.Fatal(err)
	}
	destination, err := metasqlite.Open(filepath.Join(dir, "metadata.db"), metasqlite.Namespace{Name: "vms", Tables: []meta.Table{"records"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := destination.Close(); err != nil {
			t.Errorf("close destination engine: %v", err)
		}
	}()
	if _, err := metasqlite.Convert(ctx, source, destination, "json", []meta.TableSet{{Namespace: "vms", Tables: []meta.Table{"records"}}}); err != nil {
		t.Fatal(err)
	}
	status, err := destination.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || status[0].State != "converted" || status[0].Records != 1 || status[0].Source != "json" {
		t.Fatalf("conversion status = %+v", status)
	}
}
