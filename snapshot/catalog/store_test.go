package catalog

import (
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/types"
)

func TestSnapshotCatalogPublishesAndDeletesNameAtomically(t *testing.T) {
	memory, err := metadata.NewMemory(Collections())
	if err != nil {
		t.Fatal(err)
	}
	store := New(memory)
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	record := types.Snapshot{
		ID: types.SnapshotID("223e4567-e89b-42d3-a456-426614174000"), Name: "checkpoint",
		SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000"), SourceGeneration: 4,
		ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
	}
	if err := store.Reserve(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(t.Context(), "checkpoint"); err == nil {
		t.Fatal("pending snapshot was visible")
	}
	ready, err := store.Commit(t.Context(), record.ID, 42)
	if err != nil || ready.Size != 42 || ready.Config.Name != "box" {
		t.Fatalf("Commit = %+v, %v", ready, err)
	}
	deleting, err := store.BeginDelete(t.Context(), "checkpoint")
	if err != nil || deleting.ID != record.ID {
		t.Fatalf("BeginDelete = %+v, %v", deleting, err)
	}
	if err := store.FinalizeDelete(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(t.Context(), "checkpoint"); err == nil {
		t.Fatal("deleted name still resolves")
	}
}
