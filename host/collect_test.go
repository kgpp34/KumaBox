package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/layout"
)

func TestCollectReportsHostAndRoot(t *testing.T) {
	t.Parallel()

	root := tempRoot(t)
	cniPlugins := t.TempDir()
	cniConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(cniPlugins, "bridge"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("seed cni plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cniConfig, "10-kumabox.conflist"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seed cni config: %v", err)
	}

	collector := Collector{CNIPluginDir: cniPlugins, CNIConfigDir: cniConfig}
	facts, err := collector.Collect(context.Background(), root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if facts.OS == "" || facts.Arch == "" {
		t.Errorf("OS/Arch must always be reported, got %q/%q", facts.OS, facts.Arch)
	}
	if facts.Root.Path != root.Dir() {
		t.Errorf("root path = %q, want %q", facts.Root.Path, root.Dir())
	}
	if !facts.Root.Exists || !facts.Root.Writable {
		t.Errorf("a prepared root must be reported as writable: %+v", facts.Root)
	}
	if facts.Root.TotalBytes == 0 || facts.Root.FreeBytes == 0 {
		t.Errorf("free and total space must be measured: %+v", facts.Root)
	}
	if !facts.CNI.PluginsFound || !facts.CNI.ConfigFound {
		t.Errorf("seeded CNI directories must be detected: %+v", facts.CNI)
	}
	if facts.VMM.Name != "cloud-hypervisor" {
		t.Errorf("VMM name = %q", facts.VMM.Name)
	}
}

func TestCollectDoesNotCreateTheRoot(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "not-created-yet", "root")
	root, err := layout.New(missing)
	if err != nil {
		t.Fatalf("layout.New: %v", err)
	}

	collector := Collector{CNIPluginDir: t.TempDir(), CNIConfigDir: t.TempDir()}
	facts, err := collector.Collect(context.Background(), root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if facts.Root.Exists {
		t.Errorf("Collect must never create the root: %+v", facts.Root)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("a read-only check created the root: %v", err)
	}
	if facts.Root.FreeBytes == 0 {
		t.Errorf("free space of the nearest existing ancestor must be measured: %+v", facts.Root)
	}
}

func TestCollectReportsEmptyCNIDirectoriesAsMissing(t *testing.T) {
	t.Parallel()

	collector := Collector{
		CNIPluginDir: t.TempDir(),
		CNIConfigDir: filepath.Join(t.TempDir(), "absent"),
	}
	facts, err := collector.Collect(context.Background(), tempRoot(t))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if facts.CNI.PluginsFound {
		t.Error("an empty plugin directory must not count as found")
	}
	if facts.CNI.ConfigFound {
		t.Error("an absent config directory must not count as found")
	}
}

func tempRoot(t *testing.T) layout.Root {
	t.Helper()

	root, err := layout.New(t.TempDir())
	if err != nil {
		t.Fatalf("layout.New: %v", err)
	}
	if err := root.Prepare(); err != nil {
		t.Fatalf("root.Prepare: %v", err)
	}
	return root
}
