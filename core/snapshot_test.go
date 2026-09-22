package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/snapshot"
	snapshotcatalog "github.com/kumabox/kumabox/snapshot/catalog"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

var fixedSnapshotID = types.SnapshotID("223e4567-e89b-42d3-a456-426614174000")

type fakeSnapshotReporter struct{ steps *[]string }

func (r fakeSnapshotReporter) Status(status string) error {
	*r.steps = append(*r.steps, "snapshot-status:"+status)
	return nil
}

func (r fakeSnapshotReporter) Committed(types.Snapshot) error {
	*r.steps = append(*r.steps, "snapshot-report")
	return nil
}

func newTestSnapshotService(t *testing.T) (*SnapshotService, *SandboxService, *[]string) {
	t.Helper()
	sandboxService, steps := newTestSandboxService(t, nil)
	if _, err := sandboxService.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage,
		},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := sandboxService.dependencies.catalog.(*fakeCatalog)
	catalog.record.State = types.SandboxStateRunning
	catalog.record.Generation = 4
	testRuntime(t, sandboxService).observation = vmm.Observation{
		State: vmm.ProcessRunning,
		Process: vmm.Process{
			PID: 42, StartTicks: 10, BootID: "boot", SandboxID: fixedID,
			Generation: 3, Binary: "cloud-hypervisor", APISocket: "/run/kumabox/api.sock",
		},
	}
	sandboxDir, err := sandboxService.dependencies.paths.Dir(fixedID)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureDir(sandboxDir); err != nil {
		t.Fatal(err)
	}
	cow, err := sandboxService.dependencies.paths.COW(fixedID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cow, []byte("live-cow"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := storage.Roots{
		Data: filepath.Join(t.TempDir(), "data"), Run: filepath.Join(t.TempDir(), "run"), Log: filepath.Join(t.TempDir(), "log"),
	}
	paths, err := snapshot.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	memory, err := metadata.NewMemory(snapshotcatalog.Collections())
	if err != nil {
		t.Fatal(err)
	}
	service := &SnapshotService{
		paths: paths, sandboxPaths: sandboxService.dependencies.paths,
		sandboxes: catalog, snapshots: snapshotcatalog.New(memory), runtimes: sandboxService.dependencies.runtimes,
		reporter: fakeSnapshotReporter{steps: steps}, newID: func() (types.SnapshotID, error) { return fixedSnapshotID, nil },
		now: func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }, store: memory,
		lifecycle: sandboxService,
	}
	return service, sandboxService, steps
}

func TestRestoreStopsRunningSandboxAndResumesSnapshot(t *testing.T) {
	service, sandboxService, steps := newTestSnapshotService(t)
	capture, err := service.Save(t.Context(), SaveSnapshotRequest{SandboxReference: "box", Name: "checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	*steps = nil
	record, err := service.Restore(t.Context(), "box", capture.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateRunning || record.Generation != 8 {
		t.Fatalf("restored sandbox = %+v", record)
	}
	plan := testRuntime(t, sandboxService).restorePlan
	if plan.SandboxID != fixedID || plan.Generation != 7 || plan.SnapshotDir == "" {
		t.Fatalf("restore plan = %+v", plan)
	}
	wantSequence := []string{"stopping", "stop", "stopped", "starting", "restore", "running"}
	position := 0
	for _, step := range *steps {
		if position < len(wantSequence) && step == wantSequence[position] {
			position++
		}
	}
	if position != len(wantSequence) {
		t.Fatalf("restore steps = %v, missing sequence %v", *steps, wantSequence)
	}
}

func TestRestoreFailureRetainsErrorSandbox(t *testing.T) {
	service, sandboxService, _ := newTestSnapshotService(t)
	capture, err := service.Save(t.Context(), SaveSnapshotRequest{SandboxReference: "box"})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("restore failed")
	testRuntime(t, sandboxService).restoreErr = failure
	if _, err := service.Restore(t.Context(), "box", capture.ID.String()); !errors.Is(err, failure) {
		t.Fatalf("Restore error = %v", err)
	}
	record := sandboxService.dependencies.catalog.(*fakeCatalog).record
	if record.State != types.SandboxStateError || record.Failure == nil || record.Failure.Phase != "restore VMM" {
		t.Fatalf("retained sandbox = %+v", record)
	}
}

func TestRestoreRejectsMissingCOWBeforeStoppingSandbox(t *testing.T) {
	service, sandboxService, steps := newTestSnapshotService(t)
	capture, err := service.Save(t.Context(), SaveSnapshotRequest{SandboxReference: "box"})
	if err != nil {
		t.Fatal(err)
	}
	snapshotCOW, err := service.paths.COW(capture.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(snapshotCOW); err != nil {
		t.Fatal(err)
	}
	*steps = nil
	if _, err := service.Restore(t.Context(), "box", capture.ID.String()); err == nil {
		t.Fatal("Restore accepted a snapshot without its writable disk")
	}
	record := sandboxService.dependencies.catalog.(*fakeCatalog).record
	if record.State != types.SandboxStateRunning || record.Generation != 4 {
		t.Fatalf("sandbox changed before snapshot validation: %+v", record)
	}
	for _, step := range *steps {
		if step == "stopping" || step == "stop" {
			t.Fatalf("restore stopped the sandbox before validation: %v", *steps)
		}
	}
}

func TestRestoreRecoversRetainedErrorSandbox(t *testing.T) {
	service, sandboxService, steps := newTestSnapshotService(t)
	capture, err := service.Save(t.Context(), SaveSnapshotRequest{SandboxReference: "box"})
	if err != nil {
		t.Fatal(err)
	}
	catalog := sandboxService.dependencies.catalog.(*fakeCatalog)
	catalog.record.State = types.SandboxStateError
	catalog.record.Generation = 5
	catalog.record.Failure = &types.SandboxFailure{Phase: "previous start", Message: "failed"}
	testRuntime(t, sandboxService).observation = vmm.Observation{State: vmm.ProcessAbsent}
	*steps = nil
	record, err := service.Restore(t.Context(), "box", capture.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateRunning || record.Generation != 7 || record.Failure != nil {
		t.Fatalf("restored sandbox = %+v", record)
	}
	wantSequence := []string{"cleanup", "starting", "restore", "running"}
	position := 0
	for _, step := range *steps {
		if position < len(wantSequence) && step == wantSequence[position] {
			position++
		}
	}
	if position != len(wantSequence) {
		t.Fatalf("restore steps = %v, missing sequence %v", *steps, wantSequence)
	}
}

func TestSaveSnapshotPublishesCompleteCapture(t *testing.T) {
	service, sandboxService, _ := newTestSnapshotService(t)
	record, err := service.Save(t.Context(), SaveSnapshotRequest{
		SandboxReference: "box", Name: "checkpoint/one", Description: "before upgrade",
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != fixedSnapshotID || record.SandboxID != fixedID || record.Name != "checkpoint/one" || record.Size != 5 {
		t.Fatalf("snapshot = %+v", record)
	}
	directory, err := service.paths.Dir(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "cow.raw"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("snapshot artifact %s: %v", name, err)
		}
	}
	plan := testRuntime(t, sandboxService).snapshotPlan
	if plan.Process.Generation != 3 || plan.Destination == "" || len(plan.WritableFiles) != 1 {
		t.Fatalf("snapshot plan = %+v", plan)
	}
	listed, err := service.List(t.Context())
	if err != nil || len(listed) != 1 || listed[0].ID != record.ID {
		t.Fatalf("List = %+v, %v", listed, err)
	}
}

func TestSaveSnapshotFailureCleansReservationAndStage(t *testing.T) {
	service, sandboxService, _ := newTestSnapshotService(t)
	failure := errors.New("capture failed")
	testRuntime(t, sandboxService).snapshotErr = failure
	request := SaveSnapshotRequest{SandboxReference: "box", Name: "retryable"}
	if _, err := service.Save(t.Context(), request); !errors.Is(err, failure) {
		t.Fatalf("Save error = %v", err)
	}
	if records, err := service.List(t.Context()); err != nil || len(records) != 0 {
		t.Fatalf("List after failure = %+v, %v", records, err)
	}
	stage, err := service.paths.Stage(fixedSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot stage remains: %v", err)
	}
	testRuntime(t, sandboxService).snapshotErr = nil
	if _, err := service.Save(t.Context(), request); err != nil {
		t.Fatalf("retry after compensation: %v", err)
	}
}

func TestRemoveSnapshotDeletesArtifactsAndName(t *testing.T) {
	service, _, _ := newTestSnapshotService(t)
	record, err := service.Save(t.Context(), SaveSnapshotRequest{SandboxReference: "box", Name: "remove-me"})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := service.Remove(t.Context(), "remove-me")
	if err != nil || removed.ID != record.ID {
		t.Fatalf("Remove = %+v, %v", removed, err)
	}
	if _, err := service.Inspect(t.Context(), "remove-me"); err == nil {
		t.Fatal("removed snapshot still resolves")
	}
}
