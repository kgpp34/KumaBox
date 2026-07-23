package reference

import (
	"context"
	"testing"

	"github.com/kumabox/kumabox/internal/meta"
)

func TestStoreListsExplicitTargetReferences(t *testing.T) {
	engine, err := meta.NewMemoryEngine(string(namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
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
