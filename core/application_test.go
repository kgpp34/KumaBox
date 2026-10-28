package core

import (
	"testing"

	"github.com/kumabox/kumabox/config"
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
