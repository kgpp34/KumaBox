package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/snapshot"
	snapshotcatalog "github.com/kumabox/kumabox/snapshot/catalog"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

func testMaintenance(paths snapshot.Paths, store metadata.Store, catalog snapshotCatalog) *MaintenanceService {
	state := &applicationState{paths: paths, store: store, snapshots: catalog}
	return &MaintenanceService{applicationState: state, snapshotService: &SnapshotService{applicationState: state}}
}

func TestCollectSnapshotReleasesAbandonedReservation(t *testing.T) {
	roots := gcTestRoots(t)
	paths, err := snapshot.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	store, err := metadata.NewMemory(snapshotcatalog.Collections())
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshotcatalog.New(store)
	id := types.SnapshotID("223e4567-e89b-42d3-a456-426614174000")
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	record := types.Snapshot{
		ID: id, Name: "pending", SandboxID: fixedID, SourceGeneration: 4,
		ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: time.Now().UTC(),
	}
	if err := catalog.Reserve(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if err := paths.PrepareStage(id); err != nil {
		t.Fatal(err)
	}
	service := testMaintenance(paths, store, nil)
	action, busy, err := service.collectSnapshot(t.Context(), id, nil)
	if err != nil || busy || action != "removed-stale-pending" {
		t.Fatalf("collectSnapshot = %q, %t, %v", action, busy, err)
	}
	if _, found, err := catalog.State(t.Context(), id); err != nil || found {
		t.Fatalf("pending record remains: found=%t error=%v", found, err)
	}
	stage, _ := paths.Stage(id)
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("stage remains: %v", err)
	}
}

func TestCollectSnapshotSkipsBusyReservation(t *testing.T) {
	roots := gcTestRoots(t)
	paths, err := snapshot.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	store, err := metadata.NewMemory(snapshotcatalog.Collections())
	if err != nil {
		t.Fatal(err)
	}
	id := types.SnapshotID("223e4567-e89b-42d3-a456-426614174000")
	stage, err := paths.Stage(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.PrepareStage(id); err != nil {
		t.Fatal(err)
	}
	lockPath, err := paths.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	owner := filelock.New(lockPath)
	if err := owner.Lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Unlock(t.Context()) }()
	service := testMaintenance(paths, store, nil)
	action, busy, err := service.collectSnapshot(t.Context(), id, nil)
	if err != nil || !busy || action != "" {
		t.Fatalf("collectSnapshot = %q, %t, %v", action, busy, err)
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatalf("busy stage was removed: %v", err)
	}
}

func TestCollectSnapshotPreservesReadyAndRemovesOrphan(t *testing.T) {
	roots := gcTestRoots(t)
	paths, err := snapshot.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	store, err := metadata.NewMemory(snapshotcatalog.Collections())
	if err != nil {
		t.Fatal(err)
	}
	service := testMaintenance(paths, store, nil)
	orphan := types.SnapshotID("223e4567-e89b-42d3-a456-426614174000")
	dir, _ := paths.Dir(orphan)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	action, busy, err := service.collectSnapshot(t.Context(), orphan, nil)
	if err != nil || busy || action != "removed-orphan" {
		t.Fatalf("collectSnapshot = %q, %t, %v", action, busy, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("orphan directory remains: %v", err)
	}
}

func TestCollectSnapshotRemovesStaleStageWithoutDeletingReadySnapshot(t *testing.T) {
	roots := gcTestRoots(t)
	paths, err := snapshot.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	store, err := metadata.NewMemory(snapshotcatalog.Collections())
	if err != nil {
		t.Fatal(err)
	}
	id := types.SnapshotID("223e4567-e89b-42d3-a456-426614174000")
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshotcatalog.New(store)
	if err := catalog.Reserve(t.Context(), types.Snapshot{
		ID: id, Name: "ready", SandboxID: fixedID, SourceGeneration: 4,
		ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Commit(t.Context(), id, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	published, err := paths.Dir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(published, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := paths.PrepareStage(id); err != nil {
		t.Fatal(err)
	}
	service := testMaintenance(paths, store, nil)
	action, busy, err := service.collectSnapshot(t.Context(), id, nil)
	if err != nil || busy || action != "removed-stale-stage" {
		t.Fatalf("collectSnapshot = %q, %t, %v", action, busy, err)
	}
	if state, found, err := catalog.State(t.Context(), id); err != nil || !found || !state.Ready {
		t.Fatalf("ready snapshot changed: %+v found=%t error=%v", state, found, err)
	}
	stage, _ := paths.Stage(id)
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("stage remains: %v", err)
	}
}

func TestCollectSnapshotRemovesInterruptedRestoreStage(t *testing.T) {
	roots := gcTestRoots(t)
	paths, err := snapshot.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	store, err := metadata.NewMemory(snapshotcatalog.Collections())
	if err != nil {
		t.Fatal(err)
	}
	id := types.SnapshotID("223e4567-e89b-42d3-a456-426614174000")
	file, err := paths.RestoreCOW(id, fixedID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("interrupted copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := testMaintenance(paths, store, nil)
	ids, restoreStages, err := service.discoverSnapshotArtifacts()
	if err != nil || !ids[id] || len(restoreStages[id]) != 1 {
		t.Fatalf("restore discovery: ids=%v stages=%v error=%v", ids, restoreStages, err)
	}
	action, busy, err := service.collectSnapshot(t.Context(), id, restoreStages[id])
	if err != nil || busy || action != "removed-orphan" {
		t.Fatalf("collectSnapshot = %q, %t, %v", action, busy, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("interrupted restore file remains: %v", err)
	}
}

func TestCollectSnapshotForgetsReadyRecordWithMissingDirectory(t *testing.T) {
	roots := gcTestRoots(t)
	paths, err := snapshot.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	store, err := metadata.NewMemory(snapshotcatalog.Collections())
	if err != nil {
		t.Fatal(err)
	}
	id := types.SnapshotID("223e4567-e89b-42d3-a456-426614174000")
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshotcatalog.New(store)
	if err := catalog.Reserve(t.Context(), types.Snapshot{
		ID: id, Name: "missing", SandboxID: fixedID, SourceGeneration: 4,
		ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Commit(t.Context(), id, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	service := testMaintenance(paths, store, catalog)
	action, busy, err := service.collectSnapshot(t.Context(), id, nil)
	if err != nil || busy || action != "removed-missing-dir" {
		t.Fatalf("collectSnapshot = %q, %t, %v", action, busy, err)
	}
	if _, found, err := catalog.State(t.Context(), id); err != nil || found {
		t.Fatalf("missing-dir record remains: found=%t error=%v", found, err)
	}
}

func gcTestRoots(t *testing.T) storage.Roots {
	t.Helper()
	base := t.TempDir()
	return storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
}
