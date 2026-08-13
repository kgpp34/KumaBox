package gc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/fault"
	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/resources"
	"github.com/kumabox/kumabox/internal/snapshot"
)

func TestBuildSnapshotPolicyPlan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	item := func(id, source string, age time.Duration, size int64) snapshotPolicyItem {
		accessed := now.Add(-age)
		return snapshotPolicyItem{record: &snapshot.Record{
			ID: id, Name: id, State: snapshot.StateReady, SizeBytes: size,
			CreatedAt: accessed, LastAccessedAt: accessed,
		}, sourceVMID: source}
	}
	tests := []struct {
		name           string
		items          []snapshotPolicyItem
		policy         SnapshotPolicy
		wantCandidates []string
		wantBlocked    []string
		wantBytes      int64
		wantSatisfied  bool
	}{
		{
			name: "keep latest per source",
			items: []snapshotPolicyItem{
				item("a-old", "vm-a", 3*time.Hour, 10), item("a-new", "vm-a", time.Hour, 10),
				item("b-old", "vm-b", 4*time.Hour, 10), item("b-new", "vm-b", 2*time.Hour, 10),
			},
			policy: SnapshotPolicy{KeepLast: 1}, wantCandidates: []string{"b-old", "a-old"},
			wantBytes: 20, wantSatisfied: true,
		},
		{
			name: "explicit keep zero selects every snapshot",
			items: []snapshotPolicyItem{
				item("old", "vm-a", 2*time.Hour, 10), item("new", "vm-a", time.Hour, 10),
			},
			policy: SnapshotPolicy{KeepLastSet: true}, wantCandidates: []string{"old", "new"},
			wantBytes: 0, wantSatisfied: true,
		},
		{
			name: "age and size use stable LRU order",
			items: []snapshotPolicyItem{
				item("old", "vm-a", 10*time.Hour, 30), item("middle", "vm-a", 5*time.Hour, 30), item("new", "vm-a", time.Hour, 30),
			},
			policy:         SnapshotPolicy{MaxAge: 8 * time.Hour, MaxBytes: 40},
			wantCandidates: []string{"old", "middle"}, wantBytes: 30, wantSatisfied: true,
		},
		{
			name: "references leases and unknown owners fail closed",
			items: []snapshotPolicyItem{
				func() snapshotPolicyItem {
					v := item("referenced", "vm-a", 10*time.Hour, 20)
					v.references = []reference.Record{{ID: "ref"}}
					return v
				}(),
				func() snapshotPolicyItem { v := item("leased", "vm-a", 9*time.Hour, 20); v.leased = true; return v }(),
				item("unknown", "", 8*time.Hour, 20),
			},
			policy:      SnapshotPolicy{MaxAge: time.Hour, MaxBytes: 1},
			wantBlocked: []string{"referenced", "leased", "unknown"}, wantBytes: 60, wantSatisfied: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := buildSnapshotPolicyPlan(tt.items, tt.policy, now)
			if got := candidateIDs(report.Candidates); !equalStrings(got, tt.wantCandidates) {
				t.Fatalf("candidate IDs = %v, want %v", got, tt.wantCandidates)
			}
			if got := candidateIDs(report.Blocked); !equalStrings(got, tt.wantBlocked) {
				t.Fatalf("blocked IDs = %v, want %v", got, tt.wantBlocked)
			}
			if report.EstimatedBytes != tt.wantBytes || report.TargetSatisfied != tt.wantSatisfied {
				t.Fatalf("estimated bytes=%d satisfied=%t, want %d/%t", report.EstimatedBytes, report.TargetSatisfied, tt.wantBytes, tt.wantSatisfied)
			}
		})
	}
}

func TestSnapshotPolicyGCMatchesJSONAndSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			cfg := snapshotPolicyConfig(t, backend)
			stores, err := resources.NewStoreSetForConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if stores.Metadata != nil {
				engine := stores.Metadata
				t.Cleanup(func() { _ = engine.Close() })
			}
			oldest := createPolicySnapshot(t, stores, "oldest", "vm-source", 10)
			time.Sleep(time.Millisecond)
			middle := createPolicySnapshot(t, stores, "middle", "vm-source", 20)
			time.Sleep(time.Millisecond)
			newest := createPolicySnapshot(t, stores, "newest", "vm-source", 30)
			if err := stores.References.Upsert(t.Context(), reference.Record{
				ID: "operation-oldest", SourceKind: "operation", SourceID: "op-1",
				TargetKind: "snapshot", TargetID: oldest.ID, Mode: "active",
			}); err != nil {
				t.Fatal(err)
			}
			if stores.Metadata != nil {
				if err := stores.Metadata.Close(); err != nil {
					t.Fatal(err)
				}
				stores.Metadata = nil
			}

			dryRun, err := DryRunContext(t.Context(), cfg, Options{SnapshotPolicy: &SnapshotPolicy{KeepLast: 1}})
			if err != nil {
				t.Fatal(err)
			}
			if got := candidateIDs(dryRun.SnapshotPolicy.Candidates); !equalStrings(got, []string{middle.ID}) {
				t.Fatalf("dry-run candidates = %v", got)
			}
			if got := candidateIDs(dryRun.SnapshotPolicy.Blocked); !equalStrings(got, []string{oldest.ID}) {
				t.Fatalf("dry-run blocked = %v", got)
			}

			repaired, err := RepairWithOptions(t.Context(), cfg, Options{SnapshotPolicy: &SnapshotPolicy{KeepLast: 1}})
			if err != nil {
				t.Fatal(err)
			}
			if got := candidateIDs(repaired.SnapshotPolicy.Deleted); !equalStrings(got, []string{middle.ID}) {
				t.Fatalf("deleted = %v", got)
			}
			verifyStores, err := resources.NewStoreSetForConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if verifyStores.Metadata != nil {
				defer verifyStores.Metadata.Close() //nolint:errcheck
			}
			for _, id := range []string{oldest.ID, newest.ID} {
				if _, err := verifyStores.Snapshots.Inspect(id); err != nil {
					t.Fatalf("protected snapshot %s: %v", id, err)
				}
			}
			if _, err := verifyStores.Snapshots.Inspect(middle.ID); err == nil {
				t.Fatalf("snapshot %s was not deleted", middle.ID)
			}
		})
	}
}

func TestSnapshotPolicyFailureBeforeDeleteKeepsSnapshot(t *testing.T) {
	cfg := snapshotPolicyConfig(t, "json")
	stores, err := resources.NewStoreSetForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ready := createPolicySnapshot(t, stores, "retained", "vm-source", 10)
	injected := errors.New("injected before GC delete")
	ctx := fault.WithInjector(t.Context(), fault.InjectorFunc(func(point fault.Point) error {
		if point == fault.GCBeforeDelete {
			return injected
		}
		return nil
	}))
	if _, err := RepairWithOptions(ctx, cfg, Options{SnapshotPolicy: &SnapshotPolicy{KeepLastSet: true}}); !errors.Is(err, injected) {
		t.Fatalf("RepairWithOptions() error = %v, want %v", err, injected)
	}
	if _, err := stores.Snapshots.Inspect(ready.ID); err != nil {
		t.Fatalf("snapshot changed before delete: %v", err)
	}
	report, err := RepairWithOptions(t.Context(), cfg, Options{SnapshotPolicy: &SnapshotPolicy{KeepLastSet: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(report.SnapshotPolicy.Deleted); !equalStrings(got, []string{ready.ID}) {
		t.Fatalf("retry deleted = %v, want %s", got, ready.ID)
	}
}

func snapshotPolicyConfig(t *testing.T, backend string) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(root, "data")
	cfg.Runtime.RunDir = filepath.Join(root, "run")
	cfg.Runtime.LogDir = filepath.Join(root, "log")
	cfg.Metadata.Backend = backend
	if backend == "sqlite" {
		cfg.Metadata.Path = filepath.Join(cfg.Runtime.RootDir, "metadata", "kumabox.db")
		if err := resources.InitSQLiteMetadata(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func createPolicySnapshot(t *testing.T, stores resources.StoreSet, name, source string, size int64) *snapshot.Record {
	t.Helper()
	build, err := stores.Snapshots.Reserve(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	record := build.Record()
	manifest := snapshot.Manifest{
		SchemaVersion: "kumabox.snapshot.v2", ID: record.ID, Name: record.Name,
		Type: "stopped", Consistency: "crash", Source: snapshot.Source{VMID: source},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(record.StagingDir, snapshot.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(size)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

func candidateIDs(candidates []SnapshotPolicyCandidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
