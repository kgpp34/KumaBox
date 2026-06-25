package doctor

import (
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
)

func TestRunInitializesRuntimeDirectories(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	report := Run(cfg)
	if len(report.Checks) == 0 {
		t.Fatal("expected checks")
	}

	var foundPaths bool
	for _, check := range report.Checks {
		if check.Name == "paths" {
			foundPaths = true
			if check.Status != StatusPass {
				t.Fatalf("paths status = %s: %s", check.Status, check.Message)
			}
		}
	}
	if !foundPaths {
		t.Fatal("missing paths check")
	}
}
