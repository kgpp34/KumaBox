package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestStoreReserveFinalizeAndList(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	build, err := store.Reserve(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = build.Abort() })
	if records, err := store.List(); err != nil || len(records) != 0 {
		t.Fatalf("pending list = %+v, err = %v", records, err)
	}
	if _, err := store.Inspect("first"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inspect pending error = %v, want ErrNotFound", err)
	}
	if err := os.WriteFile(filepath.Join(build.Record().StagingDir, "snapshot.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := build.SetPerformance(CaptureMetrics{PauseDurationMs: 12, PublicationDurationMs: 34}); err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(4096)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != StateReady || ready.StagingDir != "" || ready.SizeBytes != 4096 {
		t.Fatalf("finalized record = %+v", ready)
	}
	if ready.Performance == nil || ready.Performance.PauseDurationMs != 12 || ready.Performance.PublicationDurationMs != 34 {
		t.Fatalf("capture performance = %+v", ready.Performance)
	}
	if _, err := os.Stat(filepath.Join(ready.DataDir, "snapshot.json")); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil || len(records) != 1 || records[0].ID != ready.ID {
		t.Fatalf("ready list = %+v, err = %v", records, err)
	}
	inspected, err := store.Inspect(ready.ID[:10])
	if err != nil || inspected.ID != ready.ID {
		t.Fatalf("inspect prefix = %+v, err = %v", inspected, err)
	}
}

func TestStoreRecoversPreviousIndexGeneration(t *testing.T) {
	store := NewStore(t.TempDir())
	ready := createReadySnapshot(t, store, "recoverable")
	second, err := store.Reserve(context.Background(), "transient")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Abort(); err != nil {
		t.Fatal(err)
	}

	indexPath := filepath.Join(store.rootDir, "index.json")
	if err := os.WriteFile(indexPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Inspect(ready.ID)
	if err != nil {
		t.Fatalf("inspect recovered snapshot: %v", err)
	}
	if recovered.ID != ready.ID {
		t.Fatalf("recovered ID = %s, want %s", recovered.ID, ready.ID)
	}
}

func TestStoreReserveRejectsNameConflict(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	first, err := store.Reserve(context.Background(), "duplicate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Abort() })
	if _, err := store.Reserve(context.Background(), "duplicate"); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("reserve error = %v, want ErrNameConflict", err)
	}
}

func TestStoreRemoveRejectsActiveReadLease(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	ready := createReadySnapshot(t, store, "leased")
	_, lease, err := store.AcquireRead(context.Background(), ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove(ready.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("remove error = %v, want ErrInUse", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Remove(ready.Name)
	if err != nil {
		t.Fatal(err)
	}
	if removed.ID != ready.ID {
		t.Fatalf("removed = %+v", removed)
	}
	if _, err := os.Stat(ready.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot payload still exists: %v", err)
	}
}

func TestStoreAcquireReadTouchesLastAccessButPeekManifestDoesNot(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	ready := createReadySnapshot(t, store, "access-time")

	if _, err := store.PeekManifest(t.Context(), ready.ID); err == nil || !strings.Contains(err.Error(), "manifest identity") {
		// createReadySnapshot intentionally writes an empty test manifest. Peek
		// must still avoid touching the record when validation fails.
		if err == nil {
			t.Fatal("expected invalid fixture manifest")
		}
	}
	afterPeek, err := store.Inspect(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !afterPeek.LastAccessedAt.Equal(ready.LastAccessedAt) {
		t.Fatalf("peek changed last access from %s to %s", ready.LastAccessedAt, afterPeek.LastAccessedAt)
	}

	time.Sleep(time.Millisecond)
	_, lease, err := store.AcquireRead(t.Context(), ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	afterRead, err := store.Inspect(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !afterRead.LastAccessedAt.After(ready.LastAccessedAt) {
		t.Fatalf("last access = %s, want after %s", afterRead.LastAccessedAt, ready.LastAccessedAt)
	}
}

func TestStoreRemoveRejectsDurableVMDependency(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	store := NewStore(rootDir)
	ready := createReadySnapshot(t, store, "runtime-pinned")
	vmStore := vmstore.New(rootDir)
	rec, err := vmStore.Create(vmstore.CreateRequest{
		Name: "dependent", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd", RunDir: filepath.Join(rootDir, "run"), LogDir: filepath.Join(rootDir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vmStore.BeginRestore(rec.ID, ready.ID, "ondemand"); err != nil {
		t.Fatal(err)
	}
	if _, err := vmStore.CompleteRestore(rec.ID, 1234, filepath.Join(rec.RunDir, "ch.sock"), time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove(ready.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("remove error = %v, want ErrInUse", err)
	}
	if leased, err := store.IsLeased(ready.ID); err != nil || !leased {
		t.Fatalf("durable lease = %t, err = %v", leased, err)
	}
	if err := vmStore.UpdateStates([]string{rec.ID}, vmstore.StateStopped); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove(ready.ID); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRemoveRejectsHibernateSnapshot(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	store := NewStore(rootDir)
	ready := createReadySnapshot(t, store, "hibernate-pinned")
	vmStore := vmstore.New(rootDir)
	rec, err := vmStore.Create(vmstore.CreateRequest{
		Name: "hibernated", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd", RunDir: filepath.Join(rootDir, "run"), LogDir: filepath.Join(rootDir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vmStore.CompleteHibernate(rec.ID, ready.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove(ready.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("remove hibernate snapshot error = %v", err)
	}
}

func TestBuildFinalizeRequiresManifest(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	build, err := store.Reserve(context.Background(), "missing-manifest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = build.Abort() })
	if _, err := build.Finalize(0); err == nil {
		t.Fatal("expected missing manifest error")
	}
	if records, err := store.List(); err != nil || len(records) != 0 {
		t.Fatalf("failed build leaked ready record: %+v, err = %v", records, err)
	}
}

func createReadySnapshot(t *testing.T, store *Store, name string) *Record {
	t.Helper()
	build, err := store.Reserve(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build.Record().StagingDir, "snapshot.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(1)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}
