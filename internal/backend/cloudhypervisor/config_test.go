package cloudhypervisor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/config"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
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
	rec.VsockSocket = filepath.Join(rec.RunDir, "vsock.uds")

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
	if rendered.Vsock == nil || rendered.Vsock.CID != 3 || rendered.Vsock.Socket != rec.VsockSocket {
		t.Fatalf("vsock = %+v", rendered.Vsock)
	}
	if !argsContainPair(rendered.Args, "--vsock", "cid=3,socket="+rec.VsockSocket) {
		t.Fatalf("vsock arg missing: %v", rendered.Args)
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
		Metadata: &vmstore.Metadata{
			Type:       "nocloud",
			CidataDir:  filepath.Join(dir, "run", "vms", "kb_uefi", "cidata"),
			CidataDisk: filepath.Join(dir, "run", "vms", "kb_uefi", "cidata.img"),
		},
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
	if len(rendered.Disks) != 2 {
		t.Fatalf("disks = %+v", rendered.Disks)
	}
	if rendered.Disks[1].Path != rec.Metadata.CidataDisk || !rendered.Disks[1].Readonly || rendered.Disks[1].ImageType != "raw" {
		t.Fatalf("cidata disk = %+v", rendered.Disks[1])
	}
	for _, name := range []string{"meta-data", "user-data", "network-config"} {
		if _, err := os.Stat(filepath.Join(rec.Metadata.CidataDir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	if _, err := os.Stat(rec.Metadata.CidataDisk); err != nil {
		t.Fatal(err)
	}
	if !argsContainPair(rendered.Args, "--firmware", rec.Firmware) {
		t.Fatalf("firmware arg missing: %v", rendered.Args)
	}
	if !argsContainPair(rendered.Args, "--disk", "path="+rec.RootDisk+",image_type=qcow2,backing_files=on") {
		t.Fatalf("qcow2 backing files arg missing: %v", rendered.Args)
	}
	if !argsContainPair(rendered.Args, "--disk", "path="+rec.Metadata.CidataDisk+",readonly=on,image_type=raw") {
		t.Fatalf("cidata disk arg missing: %v", rendered.Args)
	}
}

func TestRenderConfigEnablesBackingFilesOnlyForWritableQcow2(t *testing.T) {
	rec := &vmstore.VMRecord{
		ID:       "kb_overlay",
		Name:     "overlay",
		Firmware: "/fixtures/CLOUDHV.fd",
		RunDir:   "/run/kumabox/vms/kb_overlay",
		LogDir:   "/var/log/kumabox/vms/kb_overlay",
		StorageConfigs: []vmstore.StorageConfig{
			{ID: "root", Role: vmstore.StorageRoleCOW, Path: "/data/root.overlay.qcow2", Format: "qcow2"},
			{ID: "layer", Role: vmstore.StorageRoleLayer, Path: "/data/layer.erofs", Readonly: true, Format: "raw"},
		},
	}

	rendered := NewConfig(config.Default(), rec)
	if !rendered.Disks[0].BackingFiles {
		t.Fatal("writable qcow2 disk did not enable backing files")
	}
	if rendered.Disks[1].BackingFiles {
		t.Fatal("read-only raw disk unexpectedly enabled backing files")
	}
	if !argsContainPair(rendered.Args, "--disk", "path=/data/root.overlay.qcow2,image_type=qcow2,backing_files=on") {
		t.Fatalf("overlay disk arg missing: %v", rendered.Args)
	}
}

func TestRenderConfigSupportsOCIStorageDisks(t *testing.T) {
	dir := t.TempDir()
	rec := &vmstore.VMRecord{
		ID:            "kb_oci",
		Name:          "oci",
		Kernel:        "/fixtures/vmlinuz",
		Initrd:        "/fixtures/initrd.img",
		KernelCmdline: "console=ttyS0 kumabox.layers={{layers}} kumabox.cow={{cow}}",
		RunDir:        filepath.Join(dir, "run", "vms", "kb_oci"),
		LogDir:        filepath.Join(dir, "logs", "vms", "kb_oci"),
		Config:        filepath.Join(dir, "run", "vms", "kb_oci", "cloud-hypervisor.json"),
		StorageConfigs: []vmstore.StorageConfig{
			{
				ID:        "layer0",
				Type:      "layer",
				Path:      "/data/oci/erofs/blobs/sha256/layer0.erofs",
				Readonly:  true,
				ImageType: "raw",
				Serial:    "kumabox-layer0",
			},
			{
				ID:        "cow",
				Type:      "cow",
				Path:      filepath.Join(dir, "run", "vms", "kb_oci", "cow.ext4"),
				ImageType: "raw",
				Serial:    "kumabox-cow",
			},
		},
		NetworkConfigs: []kbnetwork.Config{
			{
				MAC:    "5a:00:00:00:00:01",
				IfName: "eth0",
				Network: &kbnetwork.GuestInfo{
					IP:      "10.88.0.2",
					Gateway: "10.88.0.1",
					Prefix:  16,
					DNS:     []string{"1.1.1.1", "8.8.8.8"},
				},
			},
		},
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
	if len(rendered.Disks) != 2 {
		t.Fatalf("disks = %+v", rendered.Disks)
	}
	if !rendered.Disks[0].Readonly || rendered.Disks[0].Serial != "kumabox-layer0" {
		t.Fatalf("layer disk = %+v", rendered.Disks[0])
	}
	if rendered.Disks[1].Readonly || rendered.Disks[1].Serial != "kumabox-cow" {
		t.Fatalf("cow disk = %+v", rendered.Disks[1])
	}
	wantCmdline := "console=ttyS0 kumabox.layers=kumabox-layer0 kumabox.cow=kumabox-cow kumabox.hostname=oci net.ifnames=0 ip=10.88.0.2::10.88.0.1:255.255.0.0:oci:eth0:off:1.1.1.1:8.8.8.8"
	if rendered.Kernel == nil || rendered.Kernel.Cmdline != wantCmdline {
		t.Fatalf("kernel = %+v", rendered.Kernel)
	}
	if !argsContainPair(rendered.Args, "--disk", "path=/data/oci/erofs/blobs/sha256/layer0.erofs,readonly=on,image_type=raw,serial=kumabox-layer0") {
		t.Fatalf("layer disk arg missing: %v", rendered.Args)
	}
	if !argsContainPair(rendered.Args, "--disk", "path="+filepath.Join(dir, "run", "vms", "kb_oci", "cow.ext4")+",image_type=raw,serial=kumabox-cow") {
		t.Fatalf("cow disk arg missing: %v", rendered.Args)
	}
}

func TestRenderConfigIncludesNetworkDevice(t *testing.T) {
	dir := t.TempDir()
	rec := &vmstore.VMRecord{
		ID:       "kb_net",
		Name:     "net",
		RootDisk: "/fixtures/ubuntu.img",
		Firmware: "/fixtures/CLOUDHV.fd",
		CPUs:     4,
		RunDir:   filepath.Join(dir, "run", "vms", "kb_net"),
		LogDir:   filepath.Join(dir, "logs", "vms", "kb_net"),
		Config:   filepath.Join(dir, "run", "vms", "kb_net", "cloud-hypervisor.json"),
		Metadata: &vmstore.Metadata{
			Type:       "nocloud",
			CidataDir:  filepath.Join(dir, "run", "vms", "kb_net", "cidata"),
			CidataDisk: filepath.Join(dir, "run", "vms", "kb_net", "cidata.img"),
		},
		NetworkConfigs: []kbnetwork.Config{{
			ID:        "net_test",
			TAP:       "kbtaptest",
			MAC:       "02:00:00:00:00:11",
			NumQueues: 2,
			QueueSize: 256,
			Backend:   kbnetwork.ProviderCNI,
			IfName:    "eth0",
			NetnsPath: "/var/run/netns/kb_net",
			Network: &kbnetwork.GuestInfo{
				IP:      "10.88.0.2",
				Gateway: "10.88.0.1",
				Prefix:  16,
				DNS:     []string{"1.1.1.1"},
			},
		}},
	}

	if err := NewRenderer(config.Default()).RenderConfig(rec); err != nil {
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
	if len(rendered.Nets) != 1 {
		t.Fatalf("nets = %+v", rendered.Nets)
	}
	if rendered.Nets[0].TAP != "kbtaptest" || rendered.Nets[0].MAC != "02:00:00:00:00:11" {
		t.Fatalf("net = %+v", rendered.Nets[0])
	}
	if rendered.NetnsPath != "/var/run/netns/kb_net" {
		t.Fatalf("netns path = %s", rendered.NetnsPath)
	}
	if rendered.CPUs.Boot != 4 {
		t.Fatalf("cpus = %+v", rendered.CPUs)
	}
	if !argsContainPair(rendered.Args, "--cpus", "boot=4") {
		t.Fatalf("cpus arg missing: %v", rendered.Args)
	}
	if !argsContainPair(rendered.Args, "--net", "tap=kbtaptest,mac=02:00:00:00:00:11,num_queues=2,queue_size=256") {
		t.Fatalf("net arg missing: %v", rendered.Args)
	}
	networkConfig, err := os.ReadFile(filepath.Join(rec.Metadata.CidataDir, "network-config"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(networkConfig, []byte(`macaddress: "02:00:00:00:00:11"`)) ||
		!bytes.Contains(networkConfig, []byte("10.88.0.2/16")) ||
		!bytes.Contains(networkConfig, []byte("gateway4: 10.88.0.1")) {
		t.Fatalf("network-config = %s", networkConfig)
	}
}

func TestRenderConfigRejectsInvalidNetworkQueues(t *testing.T) {
	dir := t.TempDir()
	rec := &vmstore.VMRecord{
		ID:       "kb_bad_queue",
		Name:     "bad-queue",
		RootDisk: "/fixtures/ubuntu.img",
		Firmware: "/fixtures/CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run", "vms", "kb_bad_queue"),
		LogDir:   filepath.Join(dir, "logs", "vms", "kb_bad_queue"),
		Config:   filepath.Join(dir, "run", "vms", "kb_bad_queue", "cloud-hypervisor.json"),
		NetworkConfigs: []kbnetwork.Config{{
			ID:        "net_bad",
			TAP:       "kbtapbad",
			MAC:       "02:00:00:00:00:12",
			NumQueues: 1,
			Backend:   kbnetwork.ProviderHostTap,
		}},
	}

	err := NewRenderer(config.Default()).RenderConfig(rec)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("numQueues must be at least 2")) {
		t.Fatalf("render error = %v", err)
	}
}

func TestRenderConfigSkipsCidataAfterFirstBoot(t *testing.T) {
	dir := t.TempDir()
	rec := &vmstore.VMRecord{
		ID:          "kb_uefi",
		Name:        "uefi",
		RootDisk:    "/fixtures/ubuntu.img",
		Firmware:    "/fixtures/CLOUDHV.fd",
		RunDir:      filepath.Join(dir, "run", "vms", "kb_uefi"),
		LogDir:      filepath.Join(dir, "logs", "vms", "kb_uefi"),
		Config:      filepath.Join(dir, "run", "vms", "kb_uefi", "cloud-hypervisor.json"),
		FirstBooted: true,
		Metadata: &vmstore.Metadata{
			Type:       "nocloud",
			CidataDir:  filepath.Join(dir, "run", "vms", "kb_uefi", "cidata"),
			CidataDisk: filepath.Join(dir, "run", "vms", "kb_uefi", "cidata.img"),
		},
	}

	if err := NewRenderer(config.Default()).RenderConfig(rec); err != nil {
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
	if len(rendered.Disks) != 1 {
		t.Fatalf("disks = %+v", rendered.Disks)
	}
	if argsContainPair(rendered.Args, "--disk", "path="+rec.Metadata.CidataDisk+",readonly=on,image_type=raw") {
		t.Fatalf("cidata disk arg should be skipped after first boot: %v", rendered.Args)
	}
	if _, err := os.Stat(rec.Metadata.CidataDisk); !os.IsNotExist(err) {
		t.Fatalf("cidata disk should not be regenerated after first boot: %v", err)
	}
}

func argsContainPair(args []string, key, value string) bool {
	for i, arg := range args {
		if arg == key && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}
