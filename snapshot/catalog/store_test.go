package catalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/types"
)

func TestSnapshotCatalogReadsLegacyAccessTimeAndTouchesMonotonically(t *testing.T) {
	memory, err := metadata.NewMemory(Collections())
	if err != nil {
		t.Fatal(err)
	}
	store := New(memory)
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	record := types.Snapshot{
		ID: "223e4567-e89b-42d3-a456-426614174000", SandboxID: "123e4567-e89b-42d3-a456-426614174000",
		SourceGeneration: 4, ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: created,
	}
	legacy, err := json.Marshal(encode(record, true))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(legacy, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "last_accessed_at")
	legacy, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Update(t.Context(), func(writer metadata.Writer) error {
		return writer.Put(t.Context(), CollectionSnapshots, record.ID.String(), legacy)
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Resolve(t.Context(), record.ID.String())
	if err != nil || !loaded.LastAccessedAt.Equal(created) {
		t.Fatalf("legacy access time = %s, %v", loaded.LastAccessedAt, err)
	}
	touched, err := store.Touch(t.Context(), record.ID, created.Add(time.Hour))
	if err != nil || !touched.LastAccessedAt.Equal(created.Add(time.Hour)) {
		t.Fatalf("touched access time = %s, %v", touched.LastAccessedAt, err)
	}
	unchanged, err := store.Touch(t.Context(), record.ID, created)
	if err != nil || !unchanged.LastAccessedAt.Equal(touched.LastAccessedAt) {
		t.Fatalf("older touch changed access time = %s, %v", unchanged.LastAccessedAt, err)
	}
}

func TestSnapshotCatalogPublishesAndDeletesNameAtomically(t *testing.T) {
	directIO := false
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
		RegistryReference: "registry.example.test/team/guest:v1",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, SharedMemory: true, Storage: types.DefaultSandboxStorage,
			DataDisks: []types.DataDiskSpec{{Name: "db", Size: types.MinDataDiskSize, FSType: "ext4", DirectIO: &directIO}},
		},
		CreatedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
	}
	if err := store.Reserve(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(t.Context(), "checkpoint"); err == nil {
		t.Fatal("pending snapshot was visible")
	}
	ready, err := store.Commit(t.Context(), record.ID, 42, time.Now().UTC())
	if err != nil || ready.Size != 42 || ready.Config.Name != "box" || !ready.Config.SharedMemory || ready.RegistryReference != record.RegistryReference ||
		len(ready.Config.DataDisks) != 1 || ready.Config.DataDisks[0].Name != "db" ||
		ready.Config.DataDisks[0].DirectIO == nil || *ready.Config.DataDisks[0].DirectIO {
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

func TestSnapshotUsagePinsImageUntilFinalDeletion(t *testing.T) {
	memory, err := metadata.NewMemory(Collections())
	if err != nil {
		t.Fatal(err)
	}
	store := New(memory)
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	record := types.Snapshot{
		ID:        types.SnapshotID("223e4567-e89b-42d3-a456-426614174000"),
		SandboxID: types.SandboxID("123e4567-e89b-42d3-a456-426614174000"), SourceGeneration: 4,
		ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Reserve(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		if err := memory.View(t.Context(), func(reader metadata.Reader) error {
			used, err := (Usage{}).InUse(t.Context(), reader, digest)
			if err == nil && used != want {
				t.Errorf("InUse = %t, want %t", used, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(true)
	if _, err := store.Commit(t.Context(), record.ID, 42, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	check(true)
	if _, err := store.BeginDelete(t.Context(), record.ID.String()); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err := store.FinalizeDelete(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	check(false)
}
