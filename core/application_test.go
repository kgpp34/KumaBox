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
		application.Sandboxes.dependencies.store != application.Snapshots.store {
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
