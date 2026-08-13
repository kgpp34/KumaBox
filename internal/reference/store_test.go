package reference

import (
	"context"
	"testing"

	"github.com/kumabox/kumabox/internal/metastore"
)

func TestStoreListsExplicitTargetReferences(t *testing.T) {
	engine, err := metastore.NewMemoryEngine(string(namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close metadata engine: %v", err)
		}
	}()
	store := NewWithEngine(engine)
	ctx := context.Background()
	if err := store.Upsert(ctx, Record{ID: "ref-1", SourceKind: "vm", SourceID: "vm-1", TargetKind: "snapshot", TargetID: "snap-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, Record{ID: "ref-2", SourceKind: "vm", SourceID: "vm-2", TargetKind: "snapshot", TargetID: "snap-2"}); err != nil {
		t.Fatal(err)
	}
	records, err := store.ListTarget(ctx, "snapshot", "snap-1")
	if err != nil || len(records) != 1 || records[0].SourceID != "vm-1" {
		t.Fatalf("target references = %+v, err = %v", records, err)
	}
}

func TestStoreDeletesAllReferencesForSource(t *testing.T) {
	engine, err := metastore.NewMemoryEngine(string(namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close metadata engine: %v", err)
		}
	}()
	store := NewWithEngine(engine)
	ctx := context.Background()
	for _, record := range []Record{
		{ID: "snapshot-image:one", SourceKind: "snapshot", SourceID: "one", TargetKind: "image", TargetID: "image-1"},
		{ID: "snapshot-image:two", SourceKind: "snapshot", SourceID: "two", TargetKind: "image", TargetID: "image-1"},
		{ID: "vm-snapshot:vm-1:one", SourceKind: "vm", SourceID: "vm-1", TargetKind: "snapshot", TargetID: "one"},
	} {
		if err := store.Upsert(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteSource(ctx, "snapshot", "one"); err != nil {
		t.Fatal(err)
	}
	if records, err := store.ListSource(ctx, "snapshot", "one"); err != nil || len(records) != 0 {
		t.Fatalf("deleted source references = %+v, err=%v", records, err)
	}
	if records, err := store.ListTarget(ctx, "image", "image-1"); err != nil || len(records) != 1 || records[0].SourceID != "two" {
		t.Fatalf("remaining image references = %+v, err=%v", records, err)
	}
	if records, err := store.ListSource(ctx, "vm", "vm-1"); err != nil || len(records) != 1 {
		t.Fatalf("unrelated references = %+v, err=%v", records, err)
	}
}
