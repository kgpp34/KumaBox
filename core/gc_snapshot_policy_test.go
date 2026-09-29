package core

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/snapshot"
	snapshotcatalog "github.com/kumabox/kumabox/snapshot/catalog"
	"github.com/kumabox/kumabox/types"
)

func TestCollectWithPolicyPreviewsThenEvictsReadySnapshot(t *testing.T) {
	configuration := config.Default()
	configuration.Paths = gcTestRoots(t)
	service, err := OpenSnapshots(t.Context(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	id := types.SnapshotID("223e4567-e89b-42d3-a456-426614174000")
	created := time.Now().Add(-time.Hour).UTC()
	if err := service.snapshots.Reserve(t.Context(), types.Snapshot{
		ID: id, SandboxID: fixedID, SourceGeneration: 4, ImageDigest: digest,
		VMM:       types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.snapshots.Commit(t.Context(), id, 20, created); err != nil {
		t.Fatal(err)
	}
	directory, err := service.paths.Dir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	preview, err := service.CollectWithPolicy(t.Context(), SnapshotEvictionPolicy{Enabled: true, DryRun: true})
	if err != nil || len(preview.Actions) != 1 || preview.Actions[0].Action != "would-evict:lru-all" {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("preview deleted snapshot: %v", err)
	}
	collected, err := service.CollectWithPolicy(t.Context(), SnapshotEvictionPolicy{Enabled: true})
	if err != nil || len(collected.Actions) != 1 || collected.Actions[0].Action != "evicted:lru-all" {
		t.Fatalf("eviction = %+v, %v", collected, err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("snapshot directory remains: %v", err)
	}
}

func TestPickSnapshotEvictionsCombinesLimits(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	records := []types.Snapshot{
		{ID: "123e4567-e89b-42d3-a456-426614174000", LastAccessedAt: now.Add(-72 * time.Hour), Size: 40},
		{ID: "223e4567-e89b-42d3-a456-426614174000", LastAccessedAt: now.Add(-2 * time.Hour), Size: 30},
		{ID: "323e4567-e89b-42d3-a456-426614174000", LastAccessedAt: now.Add(-time.Hour), Size: 50},
	}
	selected := pickSnapshotEvictions(records, SnapshotEvictionPolicy{
		Enabled: true, KeepLast: 1, MaxAge: 24 * time.Hour, MaxSize: 60,
	}, now)
	if len(selected) != 2 || selected[0].reason != "lru-age+lru-keep+lru-size" || selected[1].reason != "lru-keep+lru-size" {
		t.Fatalf("selected snapshots = %+v", selected)
	}
	all := pickSnapshotEvictions(records, SnapshotEvictionPolicy{Enabled: true}, now)
	if len(all) != 3 || all[0].reason != "lru-all" {
		t.Fatalf("unlimited eviction = %+v", all)
	}
}

func TestEvictSnapshotRechecksAccessAndSupportsDryRun(t *testing.T) {
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
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	id := types.SnapshotID("223e4567-e89b-42d3-a456-426614174000")
	created := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	if err := catalog.Reserve(t.Context(), types.Snapshot{
		ID: id, SandboxID: fixedID, SourceGeneration: 4, ImageDigest: digest,
		VMM:       types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	selected, err := catalog.Commit(t.Context(), id, 20, created)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := paths.Dir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	service := &SnapshotService{paths: paths, store: store, snapshots: catalog}
	if acted, busy, err := service.evictSnapshot(t.Context(), selected, true); err != nil || busy || !acted {
		t.Fatalf("dry-run = %t, %t, %v", acted, busy, err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("dry-run deleted snapshot: %v", err)
	}
	updated, err := catalog.Touch(t.Context(), id, created.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if acted, busy, err := service.evictSnapshot(t.Context(), selected, false); err != nil || busy || acted {
		t.Fatalf("stale selection = %t, %t, %v", acted, busy, err)
	}
	if acted, busy, err := service.evictSnapshot(t.Context(), updated, false); err != nil || busy || !acted {
		t.Fatalf("current selection = %t, %t, %v", acted, busy, err)
	}
	if _, found, err := catalog.State(t.Context(), id); err != nil || found {
		t.Fatalf("evicted snapshot record remains: found=%t error=%v", found, err)
	}
}
