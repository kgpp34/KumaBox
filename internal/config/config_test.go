package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesFileAndFlagOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	raw := []byte(`
[runtime]
root_dir = "/from-file/root"
run_dir = "/from-file/run"
log_dir = "/from-file/log"

[backend.cloud_hypervisor]
binary = "/usr/local/bin/cloud-hypervisor"
api_socket_timeout_ms = 1234
stop_timeout_ms = 5678
`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, Overrides{
		RootDir:            "/from-flag/root",
		CloudHypervisorBin: "/from-flag/cloud-hypervisor",
	})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Runtime.RootDir != "/from-flag/root" {
		t.Fatalf("root dir = %q", cfg.Runtime.RootDir)
	}
	if cfg.Runtime.RunDir != "/from-file/run" {
		t.Fatalf("run dir = %q", cfg.Runtime.RunDir)
	}
	if cfg.Backend.CloudHypervisor.Binary != "/from-flag/cloud-hypervisor" {
		t.Fatalf("cloud-hypervisor binary = %q", cfg.Backend.CloudHypervisor.Binary)
	}
}

func TestEnsureRuntimeDirs(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	if err := EnsureRuntimeDirs(cfg); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{cfg.Runtime.RootDir, cfg.Runtime.RunDir, cfg.Runtime.LogDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", path)
		}
	}
}
