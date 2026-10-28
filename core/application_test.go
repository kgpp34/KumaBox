package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/metadata"
	snapshotcatalog "github.com/kumabox/kumabox/snapshot/catalog"
	"github.com/kumabox/kumabox/types"
)

func TestOpenApplicationSharesOneStoreAndClosesOnce(t *testing.T) {
	configuration := config.Default()
	configuration.Paths = gcTestRoots(t)
	application, err := OpenApplication(t.Context(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	if application.Snapshots.applicationState != application.Maintenance.applicationState ||
		application.Sandboxes.dependencies.store != application.Snapshots.store || application.Images.store != application.Snapshots.store {
		t.Fatal("application services do not share one metadata engine")
	}
	if err := application.Snapshots.Close(); err != nil {
		t.Fatal(err)
	}
	if err := application.Maintenance.Close(); err != nil {
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
}

func applicationSnapshot(t *testing.T, app *Application, id types.SnapshotID, created time.Time) types.Snapshot {
	t.Helper()
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	record := types.Snapshot{
		ID: id, SandboxID: fixedID, SourceGeneration: 4, ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Config:    types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		CreatedAt: created,
	}
	if err := app.state.snapshots.Reserve(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	ready, err := app.state.snapshots.Commit(t.Context(), id, 42, created)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

func TestApplicationCloseFlushesUsageBeforeClosingDatabase(t *testing.T) {
	configuration := config.Default()
	configuration.Paths = gcTestRoots(t)
	app, err := OpenApplication(t.Context(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	record := applicationSnapshot(t, app, "223e4567-e89b-42d3-a456-426614174000", time.Now().Add(-time.Hour).UTC())
	latest := record.CreatedAt.Add(time.Hour)
	if _, err := app.state.snapshots.Touch(t.Context(), record.ID, latest); err != nil {
		t.Fatal(err)
	}
	durable, err := snapshotcatalog.New(app.state.store).Resolve(t.Context(), record.ID.String())
	if err != nil || !durable.LastAccessedAt.Equal(record.CreatedAt) {
		t.Fatalf("usage was not buffered: %v %v", durable, err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenApplication(t.Context(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	loaded, err := reopened.Snapshots.Inspect(t.Context(), record.ID.String())
	if err != nil || !loaded.LastAccessedAt.Equal(latest) {
		t.Fatalf("shutdown lost usage: %v %v", loaded, err)
	}
}

func TestApplicationClosesEngineEvenWhenFinalUsageFlushFails(t *testing.T) {
	engine, err := metadata.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("flush unavailable")
	state := &applicationState{store: engine, configuration: config.Default(), flushSnapshotAccess: func(ctx context.Context) error {
		if _, deadline := ctx.Deadline(); !deadline || ctx.Err() != nil {
			t.Fatal("final flush requires a fresh bounded context")
		}
		return failure
	}}
	if err := state.close(); !errors.Is(err, failure) {
		t.Fatalf("flush failure=%v", err)
	}
	if err := engine.View(t.Context(), func(metadata.Reader) error { return nil }); err == nil {
		t.Fatal("failed flush leaked the metadata engine")
	}
}

func TestRequestReportersDoNotMutateSharedServices(t *testing.T) {
	cfg := config.Default()
	cfg.Paths = gcTestRoots(t)
	app, err := OpenApplication(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close() }()
	first, second := []string{}, []string{}
	a := app.Sandboxes.WithReporter(fakeReporter{steps: &first})
	b := app.Sandboxes.WithReporter(fakeReporter{steps: &second})
	if err := a.dependencies.reporter.Status("first"); err != nil {
		t.Fatal(err)
	}
	if err := b.dependencies.reporter.Status("second"); err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0] != "status:first" || len(second) != 1 || second[0] != "status:second" {
		t.Fatalf("reporters mixed events: %v %v", first, second)
	}
	if _, ok := app.Sandboxes.dependencies.reporter.(discardReporter); !ok {
		t.Fatal("request replaced the application reporter")
	}
	if a.dependencies.store != b.dependencies.store || a.dependencies.store != app.Images.store {
		t.Fatal("request reopened metadata")
	}
}
