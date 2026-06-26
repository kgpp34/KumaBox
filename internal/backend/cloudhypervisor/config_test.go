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

	if err := NewRenderer(cfg).RenderConfig(rec); err != nil {
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
	if rendered.Kernel == nil {
		t.Fatal("kernel config is nil")
	}
	if rendered.Kernel.Path != rec.Kernel {
		t.Fatalf("kernel path = %s", rendered.Kernel.Path)
	}
	if rendered.APISocket != filepath.Join(rec.RunDir, "ch.sock") {
		t.Fatalf("api socket = %s", rendered.APISocket)
	}
}

func TestRenderConfigSupportsFirmwareBoot(t *testing.T) {
	dir := t.TempDir()
	rec := &vmstore.VMRecord{
		ID:       "kb_uefi",
		Name:     "uefi",
		RootDisk: "/fixtures/ubuntu.img",
		Firmware: "/fixtures/CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run", "vms", "kb_uefi"),
		LogDir:   filepath.Join(dir, "logs", "vms", "kb_uefi"),
		Config:   filepath.Join(dir, "run", "vms", "kb_uefi", "cloud-hypervisor.json"),
	}

	cfg := config.Default()
	if err := NewRenderer(cfg).RenderConfig(rec); err != nil {
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
	if rendered.Firmware == nil || rendered.Firmware.Path != rec.Firmware {
		t.Fatalf("firmware = %+v", rendered.Firmware)
	}
	if rendered.Kernel != nil || rendered.Initramfs != nil {
		t.Fatalf("direct boot payload must be omitted: kernel=%+v initramfs=%+v", rendered.Kernel, rendered.Initramfs)
	}
	for i, arg := range rendered.Args {
		if arg == "--firmware" && i+1 < len(rendered.Args) && rendered.Args[i+1] == rec.Firmware {
			return
		}
	}
	t.Fatalf("firmware arg missing: %v", rendered.Args)
}
