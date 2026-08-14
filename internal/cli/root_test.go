package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/lock"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/reference"
	"github.com/kumabox/kumabox/internal/resources"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vm"
)

func newTestRootCommand(rootDir string, paths ...string) *cobra.Command {
	cfg := config.Default()
	cfg.Runtime.RootDir = rootDir
	cfg.Runtime.RunDir = filepath.Join(rootDir, "run")
	cfg.Runtime.LogDir = filepath.Join(rootDir, "log")
	if len(paths) > 0 {
		cfg.Runtime.RunDir = paths[0]
	}
	if len(paths) > 1 {
		cfg.Runtime.LogDir = paths[1]
	}
	return NewRootCommandWithConfig(cfg)
}

func TestVersionJSONCommand(t *testing.T) {
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var payload map[string]string
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["version"] == "" {
		t.Fatal("version must not be empty")
	}
}

func TestRootCommandRejectsRuntimePathFlags(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"--root-dir", "/tmp/ignored", "version"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --root-dir") {
		t.Fatalf("expected root-dir to be rejected, got %v", err)
	}
}

func TestConfiguredQEMUImgPrecedence(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Storage.QEMUImgBinary = "/configured/qemu-img"
	if got := configuredQEMUImg("", cfg); got != cfg.Storage.QEMUImgBinary {
		t.Fatalf("configuredQEMUImg() = %q, want configured binary", got)
	}
	if got := configuredQEMUImg("/command/qemu-img", cfg); got != "/command/qemu-img" {
		t.Fatalf("configuredQEMUImg() = %q, want command override", got)
	}
}

func TestDoctorInitializesConfiguredDirectories(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")

	cmd := newTestRootCommand(rootDir, runDir, logDir)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"doctor", "--json"})

	_ = cmd.Execute()

	for _, path := range []string{rootDir, runDir, logDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", path)
		}
	}

	var payload struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}

	for _, check := range payload.Checks {
		if check.Name == "paths" && check.Status == "pass" {
			return
		}
	}
	t.Fatal("doctor output did not include passing paths check")
}

func TestNetworkLSJSONReturnsEmptyListWithoutIndex(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	cmd := newTestRootCommand(rootDir)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"network", "ls", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var records []map[string]any
	if err := json.Unmarshal(out.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %d, want 0", len(records))
	}
}

func TestNetworkInspectResolvesVMName(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")
	store := vm.New(rootDir)
	rec, err := store.Create(vm.CreateRequest{
		Name:     "p2-inspect",
		RootDisk: "fixtures/base.qcow2",
		Kernel:   "fixtures/vmlinuz",
		Initrd:   "fixtures/initrd.img",
		RunDir:   runDir,
		LogDir:   logDir,
		Network:  "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := kbnetwork.Config{
		ID:          kbnetwork.NetworkID(rec.ID, 0),
		NetworkName: "default",
		TAP:         "kbtaptest",
		MAC:         "5a:00:00:00:00:01",
		Backend:     kbnetwork.ProviderHostTap,
		BridgeDev:   "kumabox0",
		Network: &kbnetwork.GuestInfo{
			IP:      "10.88.0.2",
			Gateway: "10.88.0.1",
			Prefix:  16,
			DNS:     []string{"1.1.1.1"},
		},
	}
	if _, err := store.SetNetworkConfigs(rec.ID, []kbnetwork.Config{cfg}); err != nil {
		t.Fatal(err)
	}
	if err := kbnetwork.NewStore(rootDir).UpsertRecord(kbnetwork.Record{
		ID:        cfg.ID,
		VMID:      rec.ID,
		Network:   "default",
		Provider:  kbnetwork.ProviderHostTap,
		IfName:    "eth0",
		TAP:       cfg.TAP,
		MAC:       cfg.MAC,
		BridgeDev: cfg.BridgeDev,
		IPs:       []string{"10.88.0.2/16"},
		Gateway:   "10.88.0.1",
		DNS:       []string{"1.1.1.1"},
	}); err != nil {
		t.Fatal(err)
	}

	cmd := newTestRootCommand(rootDir, runDir, logDir)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"network", "inspect", "p2-inspect", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var result struct {
		VMID       string             `json:"vmId"`
		VMName     string             `json:"vmName"`
		Interfaces []kbnetwork.Record `json:"interfaces"`
		VMConfigs  []kbnetwork.Config `json:"vmConfigs"`
		Drift      []string           `json:"drift"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.VMID != rec.ID || result.VMName != "p2-inspect" {
		t.Fatalf("unexpected inspect identity: %+v", result)
	}
	if len(result.Interfaces) != 1 || len(result.VMConfigs) != 1 || len(result.Drift) != 0 {
		t.Fatalf("unexpected inspect result: %+v", result)
	}
}

func TestCreateInspectAndPSCommands(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")

	create := newTestRootCommand(rootDir, runDir, logDir)
	create.SetArgs([]string{
		"--cloud-hypervisor-bin", "/custom/bin/cloud-hypervisor",
		"create",
		"--name", "p0-store",
		"--root-disk", "fixtures/base.qcow2",
		"--kernel", "fixtures/vmlinuz",
		"--initrd", "fixtures/initrd.img",
	})
	var createOut bytes.Buffer
	create.SetOut(&createOut)
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	var created struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		State  string `json:"state"`
		Config string `json:"config"`
	}
	if err := json.Unmarshal(createOut.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Name != "p0-store" || created.State != "created" {
		t.Fatalf("unexpected create output: %+v", created)
	}
	if created.Config == "" {
		t.Fatal("expected rendered backend config path")
	}
	if _, err := os.Stat(created.Config); err != nil {
		t.Fatal(err)
	}
	rawConfig, err := os.ReadFile(created.Config)
	if err != nil {
		t.Fatal(err)
	}
	var renderedConfig struct {
		Binary string `json:"binary"`
	}
	if err := json.Unmarshal(rawConfig, &renderedConfig); err != nil {
		t.Fatal(err)
	}
	if renderedConfig.Binary != "/custom/bin/cloud-hypervisor" {
		t.Fatalf("rendered binary = %s", renderedConfig.Binary)
	}

	inspect := newTestRootCommand(rootDir, runDir, logDir)
	inspect.SetArgs([]string{"inspect", "p0-store", "--json"})
	var inspectOut bytes.Buffer
	inspect.SetOut(&inspectOut)
	if err := inspect.Execute(); err != nil {
		t.Fatal(err)
	}

	var inspected struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(inspectOut.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.ID != created.ID {
		t.Fatalf("inspect id = %s, want %s", inspected.ID, created.ID)
	}

	ps := newTestRootCommand(rootDir, runDir, logDir)
	ps.SetArgs([]string{"ps", "--json"})
	var psOut bytes.Buffer
	ps.SetOut(&psOut)
	if err := ps.Execute(); err != nil {
		t.Fatal(err)
	}

	var records []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(psOut.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != created.ID {
		t.Fatalf("ps records = %+v", records)
	}
}

func TestNewCreateRequestPreservesRepeatedNetworks(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RootDir = filepath.Join(dir, "data")
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	req, err := newCreateRequest(createVMFlags{
		name:     "multi-net",
		rootDisk: "fixtures/base.qcow2",
		firmware: "fixtures/CLOUDHV.fd",
		cpus:     3,
		networks: []string{"cni:front", "cni:back"},
	}, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Networks) != 2 || req.Networks[0] != "cni:front" || req.Networks[1] != "cni:back" {
		t.Fatalf("networks = %#v", req.Networks)
	}
	if req.CPUs != 3 {
		t.Fatalf("cpus = %d", req.CPUs)
	}
}

func TestCreateRejectsMixedNetworkProviderFamilies(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")

	cmd := newTestRootCommand(rootDir, runDir, logDir)
	cmd.SetArgs([]string{
		"create",
		"--name", "mixed-net",
		"--root-disk", "fixtures/base.qcow2",
		"--kernel", "fixtures/vmlinuz",
		"--initrd", "fixtures/initrd.img",
		"--network", "default",
		"--network", "cni:isolated",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "same provider family") {
		t.Fatalf("create error = %v", err)
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")
	args := []string{
		"create",
		"--name", "duplicate",
		"--root-disk", "fixtures/base.qcow2",
		"--kernel", "fixtures/vmlinuz",
		"--initrd", "fixtures/initrd.img",
	}

	first := newTestRootCommand(rootDir, runDir, logDir)
	first.SetArgs(args)
	if err := first.Execute(); err != nil {
		t.Fatal(err)
	}

	second := newTestRootCommand(rootDir, runDir, logDir)
	second.SetArgs(args)
	if err := second.Execute(); err == nil {
		t.Fatal("expected duplicate name error")
	}
}

func TestCreateFirmwareBootCommand(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")

	create := newTestRootCommand(rootDir, runDir, logDir)
	create.SetArgs([]string{
		"create",
		"--name", "uefi",
		"--root-disk", "fixtures/ubuntu.img",
		"--firmware", "fixtures/CLOUDHV.fd",
	})
	var out bytes.Buffer
	create.SetOut(&out)
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	var created struct {
		Config   string `json:"config"`
		Firmware string `json:"firmware"`
		Metadata struct {
			Type       string `json:"type"`
			CidataDir  string `json:"cidataDir"`
			CidataDisk string `json:"cidataDisk"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Firmware == "" {
		t.Fatal("expected firmware in create output")
	}
	if created.Metadata.Type != "nocloud" || created.Metadata.CidataDisk == "" {
		t.Fatalf("metadata = %+v", created.Metadata)
	}

	rawConfig, err := os.ReadFile(created.Config)
	if err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Firmware *struct {
			Path string `json:"path"`
		} `json:"firmware"`
		Kernel any `json:"kernel"`
		Disks  []struct {
			Path      string `json:"path"`
			Readonly  bool   `json:"readonly"`
			ImageType string `json:"imageType"`
		} `json:"disks"`
	}
	if err := json.Unmarshal(rawConfig, &rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.Firmware == nil || rendered.Firmware.Path == "" {
		t.Fatalf("rendered firmware = %+v", rendered.Firmware)
	}
	if rendered.Kernel != nil {
		t.Fatalf("expected no direct kernel payload: %+v", rendered.Kernel)
	}
	if len(rendered.Disks) != 2 {
		t.Fatalf("disks = %+v", rendered.Disks)
	}
	if rendered.Disks[1].Path != created.Metadata.CidataDisk || !rendered.Disks[1].Readonly || rendered.Disks[1].ImageType != "raw" {
		t.Fatalf("metadata disk = %+v", rendered.Disks[1])
	}
	for _, name := range []string{"meta-data", "user-data", "network-config"} {
		if _, err := os.Stat(filepath.Join(created.Metadata.CidataDir, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
}

func TestCreateImageRefCommand(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")
	rootDisk := filepath.Join(rootDir, "cloudimg", "img_test", "base.qcow2")
	firmware := filepath.Join(dir, "fixtures", "CLOUDHV.fd")
	if err := os.MkdirAll(filepath.Dir(rootDisk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootDisk, []byte("managed image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(firmware), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firmware, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}
	image, err := imagestore.New(rootDir).Create(imagestore.CreateRequest{
		Name:   "ubuntu",
		Source: imagestore.Source{Type: "test", URI: rootDisk},
		RootDisk: imagestore.RootDisk{
			Path:             rootDisk,
			Format:           "qcow2",
			VirtualSizeBytes: 1024 * 1024,
			SHA256:           hex.EncodeToString(sha256.New().Sum(nil)),
		},
		Boot: imagestore.Boot{Mode: "uefi", Firmware: firmware},
		OS:   imagestore.OS{Family: "ubuntu", Profile: "ubuntu-cloudimg"},
	})
	if err != nil {
		t.Fatal(err)
	}

	create := newTestRootCommand(rootDir, runDir, logDir)
	create.SetArgs([]string{
		"--qemu-img-bin", fakeQEMUImgForOverlay(t, dir, rootDisk),
		"create", "ubuntu",
		"--name", "from-image",
	})
	var out bytes.Buffer
	create.SetOut(&out)
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	var created struct {
		RootDisk string `json:"rootDisk"`
		Firmware string `json:"firmware"`
		Config   string `json:"config"`
		Image    struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			RootDisk string `json:"rootDisk"`
			BootMode string `json:"bootMode"`
		} `json:"image"`
	}
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.RootDisk == rootDisk || !strings.HasSuffix(created.RootDisk, "root.overlay.qcow2") || created.Firmware != firmware {
		t.Fatalf("boot fields = root %s firmware %s", created.RootDisk, created.Firmware)
	}
	if created.Image.ID != image.ID || created.Image.Name != "ubuntu" || created.Image.RootDisk != rootDisk {
		t.Fatalf("image ref = %+v", created.Image)
	}
	if created.Image.BootMode != "uefi" {
		t.Fatalf("image boot mode = %s", created.Image.BootMode)
	}

	rawConfig, err := os.ReadFile(created.Config)
	if err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Disks []struct {
			Path string `json:"path"`
		} `json:"disks"`
	}
	if err := json.Unmarshal(rawConfig, &rendered); err != nil {
		t.Fatal(err)
	}
	if len(rendered.Disks) == 0 || rendered.Disks[0].Path != created.RootDisk {
		t.Fatalf("rendered disks = %+v", rendered.Disks)
	}
}

func TestNewCreateRequestSupportsOCIImageStorage(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")

	req, err := newOCIImageCreateRequest(createVMFlags{
		name:    "oci-vm",
		storage: "8M",
		cpus:    2,
	}, &imagestore.ImageRecord{
		ID:   "img_oci",
		Name: "oci-image",
		Boot: imagestore.Boot{
			Mode:    "direct",
			Kernel:  filepath.Join(dir, "vmlinuz"),
			Initrd:  filepath.Join(dir, "initrd.img"),
			Cmdline: "kumabox.layers={{layers}} kumabox.cow={{cow}}",
		},
		OCI: &imagestore.OCI{
			DigestRef: "index.docker.io/kumabox/ubuntu@sha256:" + strings.Repeat("b", 64),
			Layers: []imagestore.OCILayer{
				{
					Index:  0,
					Digest: "sha256:" + strings.Repeat("a", 64),
					EROFS: &imagestore.EROFSLayer{
						Path:      filepath.Join(dir, "layer0.erofs"),
						SizeBytes: 4096,
					},
				},
			},
		},
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if req.RootDisk != "" || req.Kernel == "" || req.Initrd == "" || req.KernelCmdline == "" {
		t.Fatalf("unexpected boot request: %+v", req)
	}
	if len(req.StorageConfigs) != 2 {
		t.Fatalf("storage configs = %+v", req.StorageConfigs)
	}
	if req.StorageConfigs[0].Role != vm.StorageRoleLayer || !req.StorageConfigs[0].Readonly || req.StorageConfigs[0].Serial != "kumabox-layer0" {
		t.Fatalf("layer storage = %+v", req.StorageConfigs[0])
	}
	if req.StorageConfigs[1].Role != vm.StorageRoleCOW || req.StorageConfigs[1].VirtualSizeBytes != 8*1024*1024 || req.StorageConfigs[1].Serial != "kumabox-cow" || req.StorageConfigs[1].Base == nil {
		t.Fatalf("cow storage = %+v", req.StorageConfigs[1])
	}
	if req.StorageConfigs[1].Base.Digest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("base digest = %q", req.StorageConfigs[1].Base.Digest)
	}
	if len(req.Networks) != 1 || req.Networks[0] != "cni:kumabox" {
		t.Fatalf("OCI default network = %#v", req.Networks)
	}
}

func TestLogsCommandTailsVMLogs(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")

	create := newTestRootCommand(rootDir, runDir, logDir)
	create.SetArgs([]string{
		"create",
		"--name", "loggy",
		"--root-disk", "fixtures/base.qcow2",
		"--kernel", "fixtures/vmlinuz",
		"--initrd", "fixtures/initrd.img",
	})
	var createOut bytes.Buffer
	create.SetOut(&createOut)
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	var created struct {
		LogDir string `json:"logDir"`
	}
	if err := json.Unmarshal(createOut.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(created.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.LogDir, "cloud-hypervisor.stdout.log"), []byte("line-1\nline-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.LogDir, "cloud-hypervisor.stderr.log"), []byte("err-1\nerr-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.LogDir, "console.log"), []byte("console-1\nconsole-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logs := newTestRootCommand(rootDir, runDir, logDir)
	logs.SetArgs([]string{"logs", "loggy", "--tail", "1"})
	var logsOut bytes.Buffer
	logs.SetOut(&logsOut)
	if err := logs.Execute(); err != nil {
		t.Fatal(err)
	}

	got := logsOut.String()
	if strings.Contains(got, "==>") {
		t.Fatalf("single-source logs should not include section headers: %s", got)
	}
	if strings.Contains(got, "console-1") || !strings.Contains(got, "console-2") {
		t.Fatalf("logs output did not tail console: %s", got)
	}

	vmmLogs := newTestRootCommand(rootDir, runDir, logDir)
	vmmLogs.SetArgs([]string{"logs", "loggy", "--source", "vmm", "--tail", "1"})
	var vmmLogsOut bytes.Buffer
	vmmLogs.SetOut(&vmmLogsOut)
	if err := vmmLogs.Execute(); err != nil {
		t.Fatal(err)
	}

	vmmGot := vmmLogsOut.String()
	if !strings.Contains(vmmGot, "==> cloud-hypervisor.stdout.log <==") {
		t.Fatalf("logs output missing stdout header: %s", vmmGot)
	}
	if strings.Contains(vmmGot, "line-1") || !strings.Contains(vmmGot, "line-2") {
		t.Fatalf("logs output did not tail stdout: %s", vmmGot)
	}
	if strings.Contains(vmmGot, "err-1") || !strings.Contains(vmmGot, "err-2") {
		t.Fatalf("logs output did not tail stderr: %s", vmmGot)
	}
}

func TestDeleteCommandRemovesVMRecord(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")
	rootDisk := filepath.Join(dir, "fixtures", "base.qcow2")
	if err := os.MkdirAll(filepath.Dir(rootDisk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootDisk, []byte("root disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	create := newTestRootCommand(rootDir, runDir, logDir)
	create.SetArgs([]string{
		"create",
		"--name", "delete-cli",
		"--root-disk", rootDisk,
		"--kernel", "fixtures/vmlinuz",
		"--initrd", "fixtures/initrd.img",
	})
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	del := newTestRootCommand(rootDir, runDir, logDir)
	del.SetArgs([]string{"delete", "delete-cli"})
	var delOut bytes.Buffer
	del.SetOut(&delOut)
	if err := del.Execute(); err != nil {
		t.Fatal(err)
	}
	var deleted struct {
		Name     string `json:"name"`
		RootDisk string `json:"rootDisk"`
	}
	if err := json.Unmarshal(delOut.Bytes(), &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.Name != "delete-cli" || deleted.RootDisk != rootDisk {
		t.Fatalf("deleted payload = %+v", deleted)
	}
	if _, err := os.Stat(rootDisk); err != nil {
		t.Fatalf("root disk should remain: %v", err)
	}

	ps := newTestRootCommand(rootDir, runDir, logDir)
	ps.SetArgs([]string{"ps", "--json"})
	var psOut bytes.Buffer
	ps.SetOut(&psOut)
	if err := ps.Execute(); err != nil {
		t.Fatal(err)
	}
	var records []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(psOut.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no records after delete, got %+v", records)
	}
}

func TestDeleteCommandBestEffortBatchResult(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")
	store := vm.New(rootDir)
	wantIDs := make([]string, 0, 2)

	for _, name := range []string{"batch-a", "batch-b"} {
		record, err := store.Create(vm.CreateRequest{
			Name:     name,
			RootDisk: filepath.Join(dir, name+".qcow2"),
			Kernel:   "vmlinuz",
			Initrd:   "initrd.img",
			RunDir:   filepath.Join(runDir, name),
			LogDir:   filepath.Join(logDir, name),
		})
		if err != nil {
			t.Fatal(err)
		}
		wantIDs = append(wantIDs, record.ID)
	}

	cmd := newTestRootCommand(rootDir, runDir, logDir)
	cmd.SetArgs([]string{"delete", "batch-a", "missing", "batch-b", "--concurrency", "2"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "delete: VM missing") {
		t.Fatalf("error = %v", err)
	}

	var result struct {
		Succeeded []string `json:"succeeded"`
		Failed    []struct {
			Ref   string `json:"ref"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Succeeded) != 2 || result.Succeeded[0] != wantIDs[0] || result.Succeeded[1] != wantIDs[1] {
		t.Fatalf("succeeded = %+v", result.Succeeded)
	}
	if len(result.Failed) != 1 || result.Failed[0].Ref != "missing" || result.Failed[0].Error == "" {
		t.Fatalf("failed = %+v", result.Failed)
	}
	if records, err := store.List(); err != nil || len(records) != 0 {
		t.Fatalf("remaining records = %+v, error = %v", records, err)
	}
}

func TestLifecycleCommandRejectsNegativeConcurrency(t *testing.T) {
	cmd := newTestRootCommand(t.TempDir())
	cmd.SetArgs([]string{"start", "vm-a", "--concurrency", "-1"})
	err := cmd.Execute()
	if err == nil || err.Error() != "concurrency must be greater than or equal to zero" {
		t.Fatalf("error = %v", err)
	}
}

func TestLifecycleCommandsAcceptBatchAndExposeConcurrency(t *testing.T) {
	opts := &rootOptions{}
	commands := []*cobra.Command{
		newStartCommand(opts),
		newStopCommand(opts),
		newPauseCommand(opts),
		newResumeCommand(opts),
		newDeleteCommand(opts),
	}
	for _, cmd := range commands {
		t.Run(cmd.Name(), func(t *testing.T) {
			if err := cmd.Args(cmd, []string{"vm-a", "vm-b"}); err != nil {
				t.Fatalf("batch args rejected: %v", err)
			}
			if cmd.Flags().Lookup("concurrency") == nil {
				t.Fatal("concurrency flag is missing")
			}
		})
	}
}

func TestGCDryRunCommandReportsCandidates(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")
	orphan := filepath.Join(runDir, "vms", "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := newTestRootCommand(rootDir, runDir, logDir)
	cmd.SetArgs([]string{
		"gc",
		"--dry-run",
		"--json",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var payload struct {
		DryRun     bool `json:"dryRun"`
		Candidates []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.DryRun {
		t.Fatal("expected dryRun true")
	}
	if len(payload.Candidates) != 1 {
		t.Fatalf("candidates = %+v", payload.Candidates)
	}
	if payload.Candidates[0].Path != orphan || payload.Candidates[0].Type != "orphan_run_dir" {
		t.Fatalf("candidate = %+v", payload.Candidates[0])
	}
}

func TestGCSnapshotPolicyFlags(t *testing.T) {
	dir := t.TempDir()
	cmd := newTestRootCommand(filepath.Join(dir, "data"), filepath.Join(dir, "run"), filepath.Join(dir, "log"))
	cmd.SetArgs([]string{
		"gc", "--dry-run", "--json",
		"--snapshot-keep", "0",
		"--snapshot-max-age", "168h",
		"--snapshot-max-bytes", "20G",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		SnapshotPolicy struct {
			Policy struct {
				KeepLast int           `json:"keepLast"`
				MaxAge   time.Duration `json:"maxAge"`
				MaxBytes int64         `json:"maxBytes"`
			} `json:"policy"`
			TargetSatisfied bool `json:"targetSatisfied"`
		} `json:"snapshotPolicy"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SnapshotPolicy.Policy.KeepLast != 0 ||
		payload.SnapshotPolicy.Policy.MaxAge != 168*time.Hour ||
		payload.SnapshotPolicy.Policy.MaxBytes != 20<<30 ||
		!payload.SnapshotPolicy.TargetSatisfied {
		t.Fatalf("snapshot policy = %+v", payload.SnapshotPolicy)
	}
}

func TestGCSnapshotPolicyRejectsInvalidFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "negative keep", args: []string{"gc", "--dry-run", "--snapshot-keep", "-1"}, want: "--snapshot-keep must not be negative"},
		{name: "invalid bytes", args: []string{"gc", "--dry-run", "--snapshot-max-bytes", "none"}, want: "--snapshot-max-bytes must be a positive size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newTestRootCommand(t.TempDir())
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestImageListAndInspectCommands(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")

	listEmpty := newTestRootCommand(rootDir)
	listEmpty.SetArgs([]string{"image", "ls", "--json"})
	var emptyOut bytes.Buffer
	listEmpty.SetOut(&emptyOut)
	if err := listEmpty.Execute(); err != nil {
		t.Fatal(err)
	}
	var empty []any
	if err := json.Unmarshal(emptyOut.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected empty image list, got %+v", empty)
	}

	created, err := imagestore.New(rootDir).Create(imagestore.CreateRequest{
		Name:   "ubuntu",
		Source: imagestore.Source{Type: "test", URI: "fixtures/ubuntu.img"},
		RootDisk: imagestore.RootDisk{
			Path:   "base.qcow2",
			Format: "qcow2",
		},
		Boot: imagestore.Boot{Mode: "uefi", Firmware: "CLOUDHV.fd"},
		OS:   imagestore.OS{Family: "ubuntu", Profile: "ubuntu-cloudimg"},
	})
	if err != nil {
		t.Fatal(err)
	}

	inspect := newTestRootCommand(rootDir)
	inspect.SetArgs([]string{"image", "inspect", "ubuntu", "--json"})
	var inspectOut bytes.Buffer
	inspect.SetOut(&inspectOut)
	if err := inspect.Execute(); err != nil {
		t.Fatal(err)
	}
	var inspected struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		RootDisk struct {
			Format string `json:"format"`
		} `json:"rootDisk"`
	}
	if err := json.Unmarshal(inspectOut.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.ID != created.ID || inspected.Name != "ubuntu" || inspected.RootDisk.Format != "qcow2" {
		t.Fatalf("inspect image = %+v", inspected)
	}
}

func TestImageRemoveRejectsReferencedImage(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")
	basePath := filepath.Join(rootDir, "cloudimg", "img_test", "base.qcow2")
	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(basePath, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := imagestore.New(rootDir).Create(imagestore.CreateRequest{
		Name:   "ubuntu",
		Source: imagestore.Source{Type: "test", URI: "fixtures/ubuntu.img"},
		RootDisk: imagestore.RootDisk{
			Path:             basePath,
			Format:           "qcow2",
			VirtualSizeBytes: 1024 * 1024,
			SHA256:           strings.Repeat("a", 64),
		},
		Boot: imagestore.Boot{Mode: "uefi", Firmware: "CLOUDHV.fd"},
		OS:   imagestore.OS{Family: "ubuntu", Profile: "ubuntu-cloudimg"},
	})
	if err != nil {
		t.Fatal(err)
	}

	create := newTestRootCommand(rootDir, runDir, logDir)
	create.SetArgs([]string{
		"--qemu-img-bin", fakeQEMUImgForOverlay(t, dir, image.RootDisk.Path),
		"create", "ubuntu",
		"--name", "ref",
	})
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	rm := newTestRootCommand(rootDir, runDir, logDir)
	rm.SetArgs([]string{"image", "rm", "ubuntu"})
	if err := rm.Execute(); !errors.Is(err, imagestore.ErrImageInUse) {
		t.Fatalf("expected ErrImageInUse, got %v", err)
	}
	if _, err := imagestore.New(rootDir).Inspect(image.ID); err != nil {
		t.Fatalf("referenced image should remain: %v", err)
	}

	del := newTestRootCommand(rootDir, runDir, logDir)
	del.SetArgs([]string{"delete", "ref"})
	if err := del.Execute(); err != nil {
		t.Fatal(err)
	}

	rm = newTestRootCommand(rootDir, runDir, logDir)
	rm.SetArgs([]string{"image", "rm", "ubuntu"})
	var rmOut bytes.Buffer
	rm.SetOut(&rmOut)
	if err := rm.Execute(); err != nil {
		t.Fatal(err)
	}
	var removed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rmOut.Bytes(), &removed); err != nil {
		t.Fatal(err)
	}
	if removed.ID != image.ID {
		t.Fatalf("removed id = %s, want %s", removed.ID, image.ID)
	}
}

func TestImageRemoveBestEffortBatch(t *testing.T) {
	rootDir := t.TempDir()
	store := imagestore.New(rootDir)
	created := make([]*imagestore.ImageRecord, 0, 2)
	for _, name := range []string{"batch-image-a", "batch-image-b"} {
		record, err := store.Create(imagestore.CreateRequest{
			Name: name, Source: imagestore.Source{Type: "test", URI: name},
			RootDisk: imagestore.RootDisk{Path: name + ".qcow2", Format: "qcow2"},
			Boot:     imagestore.Boot{Mode: "uefi", Firmware: "CLOUDHV.fd"},
		})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, record)
	}
	cmd := newTestRootCommand(rootDir)
	cmd.SetArgs([]string{"image", "rm", "batch-image-a", "missing", "batch-image-b", "batch-image-a", "--concurrency", "2"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "remove image") {
		t.Fatalf("error = %v", err)
	}
	var result struct {
		Succeeded []*imagestore.ImageRecord `json:"succeeded"`
		Failed    []resourceBatchFailure    `json:"failed"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Succeeded) != 2 || result.Succeeded[0].ID != created[0].ID || result.Succeeded[1].ID != created[1].ID {
		t.Fatalf("succeeded = %+v", result.Succeeded)
	}
	if len(result.Failed) != 1 || result.Failed[0].Ref != "missing" {
		t.Fatalf("failed = %+v", result.Failed)
	}
}

func TestImagePullBatchRequiresOneNamePerURL(t *testing.T) {
	cmd := newTestRootCommand(t.TempDir())
	cmd.SetArgs([]string{
		"image", "pull", "https://example.invalid/a.img", "https://example.invalid/b.img",
		"--name", "only-one", "--firmware", "firmware.fd",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "provide one --name for each URL") {
		t.Fatalf("error = %v", err)
	}
}

func TestImageAndSnapshotBatchCommandsExposeConcurrency(t *testing.T) {
	opts := &rootOptions{}
	commands := []*cobra.Command{newImagePullCommand(opts), newImageRMCommand(opts), newSnapshotRMCommand(opts)}
	for _, cmd := range commands {
		t.Run(cmd.CommandPath(), func(t *testing.T) {
			if err := cmd.Args(cmd, []string{"first", "second"}); err != nil {
				t.Fatalf("batch args rejected: %v", err)
			}
			if cmd.Flags().Lookup("concurrency") == nil {
				t.Fatal("concurrency flag is missing")
			}
		})
	}
}

func TestSnapshotRemoveBestEffortBatch(t *testing.T) {
	rootDir := t.TempDir()
	store := snapshot.NewStore(rootDir)
	created := make([]*snapshot.Record, 0, 2)
	for _, name := range []string{"batch-snapshot-a", "batch-snapshot-b"} {
		build, err := store.Reserve(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		pending := build.Record()
		manifest := snapshot.Manifest{
			SchemaVersion: "kumabox.snapshot.v2", ID: pending.ID, Name: pending.Name,
			Type: "stopped", Consistency: "crash", Source: snapshot.Source{VMID: "vm-source"},
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pending.StagingDir, snapshot.ManifestFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		record, err := build.Finalize(int64(len(raw)))
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, record)
	}
	cmd := newTestRootCommand(rootDir)
	cmd.SetArgs([]string{"snapshot", "rm", "batch-snapshot-a", "missing", "batch-snapshot-b", "--concurrency", "2"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "remove snapshot") {
		t.Fatalf("error = %v", err)
	}
	var result struct {
		Succeeded []*snapshot.Record     `json:"succeeded"`
		Failed    []resourceBatchFailure `json:"failed"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Succeeded) != 2 || result.Succeeded[0].ID != created[0].ID || result.Succeeded[1].ID != created[1].ID {
		t.Fatalf("succeeded = %+v", result.Succeeded)
	}
	if len(result.Failed) != 1 || result.Failed[0].Ref != "missing" {
		t.Fatalf("failed = %+v", result.Failed)
	}
}

func TestImageRemoveRechecksReferencesAfterEntityLock(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			testImageRemoveRechecksReferencesAfterEntityLock(t, backend)
		})
	}
}

func testImageRemoveRechecksReferencesAfterEntityLock(t *testing.T, backend string) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	cfg := config.Default()
	cfg.Runtime.RootDir = rootDir
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")
	cfg.Metadata.Backend = backend
	if backend == "sqlite" {
		cfg.Metadata.Path = filepath.Join(rootDir, "metadata", "kumabox.db")
		if err := resources.InitSQLiteMetadata(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	stores, err := resources.NewStoreSetForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stores.Metadata != nil {
		t.Cleanup(func() { _ = stores.Metadata.Close() })
	}
	image, err := stores.Images.Create(imagestore.CreateRequest{
		Name:   "ubuntu",
		Source: imagestore.Source{Type: "test", URI: "fixtures/ubuntu.img"},
		RootDisk: imagestore.RootDisk{
			Path: filepath.Join(rootDir, "cloudimg", "base.qcow2"), Format: "qcow2",
		},
		Boot: imagestore.Boot{Mode: "uefi", Firmware: "CLOUDHV.fd"},
	})
	if err != nil {
		t.Fatal(err)
	}

	guard := lock.NewGuard(rootDir)
	mutation, err := guard.BeginMutation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mutation.Release() })
	imageLock, err := guard.LockEntity(t.Context(), lock.EntityImage, image.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = imageLock.Release() })

	rm := NewRootCommandWithConfig(cfg)
	rm.SetArgs([]string{"image", "rm", image.ID})
	result := make(chan error, 1)
	go func() { result <- rm.Execute() }()

	vm, err := stores.VM.Create(vm.CreateRequest{
		Name: "late-reference", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
		Image:  &vm.ImageRef{ID: image.ID, Name: image.Name},
		RunDir: cfg.Runtime.RunDir, LogDir: cfg.Runtime.LogDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := imageLock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := mutation.Release(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-result:
		if !errors.Is(err, imagestore.ErrImageInUse) {
			t.Fatalf("image remove error = %v, want ErrImageInUse", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("image remove did not resume after entity lock released")
	}
	if _, err := stores.Images.Inspect(image.ID); err != nil {
		t.Fatalf("newly referenced image was removed: %v", err)
	}
	if _, err := stores.VM.Inspect(vm.ID); err != nil {
		t.Fatalf("late VM reference was not persisted: %v", err)
	}
}

func TestImageRemovePrunesDanglingExplicitReferences(t *testing.T) {
	rootDir := t.TempDir()
	basePath := filepath.Join(rootDir, "cloudimg", "img_test", "base.qcow2")
	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(basePath, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	image, err := imagestore.New(rootDir).Create(imagestore.CreateRequest{
		Name:   "ubuntu",
		Source: imagestore.Source{Type: "test", URI: "fixtures/ubuntu.img"},
		RootDisk: imagestore.RootDisk{
			Path: basePath, Format: "qcow2", VirtualSizeBytes: 1024 * 1024, SHA256: strings.Repeat("a", 64),
		},
		Boot: imagestore.Boot{Mode: "uefi", Firmware: "CLOUDHV.fd"},
		OS:   imagestore.OS{Family: "ubuntu", Profile: "ubuntu-cloudimg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	references := reference.New(rootDir)
	if err := references.Upsert(context.Background(), reference.Record{
		ID: "snapshot-image:deleted", SourceKind: "snapshot", SourceID: "deleted",
		TargetKind: "image", TargetID: image.ID, Mode: "base",
	}); err != nil {
		t.Fatal(err)
	}

	rm := newTestRootCommand(rootDir)
	rm.SetArgs([]string{"image", "rm", image.ID})
	if err := rm.Execute(); err != nil {
		t.Fatal(err)
	}
	remaining, err := references.ListTarget(context.Background(), "image", image.ID)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("dangling references = %+v, err=%v", remaining, err)
	}
}

func fakeQEMUImgForOverlay(t *testing.T, dir, backing string) string {
	t.Helper()
	path := filepath.Join(dir, "qemu-img-overlay")
	script := "#!/bin/sh\nset -eu\ncase \"$1\" in\n" +
		"create) for last do :; done; : > \"$last\" ;;\n" +
		"info) printf '%s\\n' '{\"format\":\"qcow2\",\"backing-filename\":\"" + backing + "\",\"virtual-size\":1048576}' ;;\n" +
		"*) exit 2 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImageImportCommand(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	source := filepath.Join(dir, "fixtures", "jammy-server-cloudimg-amd64.img")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("cloud image"), 0o644); err != nil {
		t.Fatal(err)
	}
	firmware := filepath.Join(dir, "fixtures", "CLOUDHV.fd")
	if err := os.WriteFile(firmware, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}
	qemuImg := fakeQemuImgForCLI(t, dir, "qcow2", 4096, 11)

	importCmd := newTestRootCommand(rootDir)
	importCmd.SetArgs([]string{
		"image", "import", source,
		"--name", "ubuntu",
		"--firmware", firmware,
		"--qemu-img", qemuImg,
	})
	var importOut bytes.Buffer
	importCmd.SetOut(&importOut)
	if err := importCmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var imported struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Source struct {
			Type string `json:"type"`
			URI  string `json:"uri"`
		} `json:"source"`
		RootDisk struct {
			Path   string `json:"path"`
			Format string `json:"format"`
		} `json:"rootDisk"`
		Boot struct {
			Mode     string `json:"mode"`
			Firmware string `json:"firmware"`
		} `json:"boot"`
	}
	if err := json.Unmarshal(importOut.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.ID == "" || imported.Name != "ubuntu" || imported.Source.Type != "local-file" {
		t.Fatalf("imported = %+v", imported)
	}
	if imported.RootDisk.Format != "qcow2" || imported.RootDisk.Path == source {
		t.Fatalf("root disk = %+v", imported.RootDisk)
	}
	if imported.Boot.Mode != "uefi" || imported.Boot.Firmware != firmware {
		t.Fatalf("boot = %+v", imported.Boot)
	}

	inspect := newTestRootCommand(rootDir)
	inspect.SetArgs([]string{"image", "inspect", "ubuntu", "--json"})
	var inspectOut bytes.Buffer
	inspect.SetOut(&inspectOut)
	if err := inspect.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inspectOut.String(), imported.ID) {
		t.Fatalf("inspect output missing imported id: %s", inspectOut.String())
	}
}

func TestImagePullCommand(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	content := []byte("pulled cloud image")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()

	firmware := filepath.Join(dir, "fixtures", "CLOUDHV.fd")
	if err := os.MkdirAll(filepath.Dir(firmware), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firmware, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	qemuImg := fakeQemuImgForCLI(t, dir, "qcow2", 4096, int64(len(content)))

	pullCmd := newTestRootCommand(rootDir)
	pullCmd.SetArgs([]string{
		"image", "pull", server.URL + "/jammy-server-cloudimg-amd64.img",
		"--name", "ubuntu-pull",
		"--firmware", firmware,
		"--qemu-img", qemuImg,
		"--sha256", hex.EncodeToString(sum[:]),
	})
	var pullOut bytes.Buffer
	pullCmd.SetOut(&pullOut)
	if err := pullCmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var pulled struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Source struct {
			Type string `json:"type"`
			URI  string `json:"uri"`
		} `json:"source"`
		RootDisk struct {
			Path   string `json:"path"`
			Format string `json:"format"`
			SHA256 string `json:"sha256"`
		} `json:"rootDisk"`
	}
	if err := json.Unmarshal(pullOut.Bytes(), &pulled); err != nil {
		t.Fatal(err)
	}
	if pulled.ID == "" || pulled.Name != "ubuntu-pull" || pulled.Source.Type != "url" {
		t.Fatalf("pulled = %+v", pulled)
	}
	if pulled.RootDisk.Format != "qcow2" || pulled.RootDisk.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("root disk = %+v", pulled.RootDisk)
	}
	if _, err := os.Stat(pulled.RootDisk.Path); err != nil {
		t.Fatalf("pulled root disk missing: %v", err)
	}

	inspect := newTestRootCommand(rootDir)
	inspect.SetArgs([]string{"image", "inspect", "ubuntu-pull", "--json"})
	var inspectOut bytes.Buffer
	inspect.SetOut(&inspectOut)
	if err := inspect.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inspectOut.String(), pulled.ID) {
		t.Fatalf("inspect output missing pulled id: %s", inspectOut.String())
	}
}

func fakeQemuImgForCLI(t *testing.T, dir, format string, virtualSize, actualSize int64) string {
	t.Helper()
	path := filepath.Join(dir, "qemu-img")
	script := "#!/bin/sh\n" +
		"printf '{\"format\":\"" + format + "\",\"virtual-size\":" + strconv.FormatInt(virtualSize, 10) + ",\"actual-size\":" + strconv.FormatInt(actualSize, 10) + "}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
