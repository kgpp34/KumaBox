package cloudhypervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestRenderConfigWritesResolvedPaths(t *testing.T) {
	dir := t.TempDir()
	rec := &vmstore.VMRecord{
		ID:       "kb_test",
		Name:     "test",
		RootDisk: "/fixtures/base.qcow2",
		Kernel:   "/fixtures/vmlinuz",
		Initrd:   "/fixtures/initrd.img",
		RunDir:   filepath.Join(dir, "run", "vms", "kb_test"),
		LogDir:   filepath.Join(dir, "logs", "vms", "kb_test"),
		Config:   filepath.Join(dir, "run", "vms", "kb_test", "cloud-hypervisor.json"),
	}

	cfg := config.Default()
	cfg.Backend.CloudHypervisor.Binary = "/usr/local/bin/cloud-hypervisor"

	if err := RenderConfig(cfg, rec); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(rec.Config)
	if err != nil {
		t.Fatal(err)
	}

	var rendered Config
	if err := json.Unmarshal(raw, &rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.Binary != "/usr/local/bin/cloud-hypervisor" {
		t.Fatalf("binary = %s", rendered.Binary)
	}
	if rendered.Kernel.Path != rec.Kernel {
		t.Fatalf("kernel path = %s", rendered.Kernel.Path)
	}
	if rendered.APISocket != filepath.Join(rec.RunDir, "ch.sock") {
		t.Fatalf("api socket = %s", rendered.APISocket)
	}
}
