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

[network]
mode = "host-tap"
default = "default"
bridge = "kb-test0"
cidr = "10.99.0.0/16"
gateway = "10.99.0.1"
dns = ["9.9.9.9"]
tap_prefix = "kbtest"
nat_backend = "nft"
cni_config_dir = "/tmp/cni/net.d"
cni_bin_dir = "/tmp/cni/bin"
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
	if cfg.Network.Bridge != "kb-test0" {
		t.Fatalf("network bridge = %q", cfg.Network.Bridge)
	}
	if cfg.Network.NATBackend != "nft" {
		t.Fatalf("network nat backend = %q", cfg.Network.NATBackend)
	}
	if len(cfg.Network.DNS) != 1 || cfg.Network.DNS[0] != "9.9.9.9" {
		t.Fatalf("network dns = %#v", cfg.Network.DNS)
	}
}

func TestDefaultNetworkConfig(t *testing.T) {
	cfg := Default()
	if cfg.Network.Mode != "host-tap" {
		t.Fatalf("network mode = %q", cfg.Network.Mode)
	}
	if cfg.Network.Bridge != "kumabox0" {
		t.Fatalf("network bridge = %q", cfg.Network.Bridge)
	}
	if cfg.Network.CIDR == "" || cfg.Network.Gateway == "" || cfg.Network.TapPrefix == "" {
		t.Fatalf("incomplete default network config: %+v", cfg.Network)
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
