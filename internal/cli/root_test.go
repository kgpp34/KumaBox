package cli

import (
	"bytes"
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

	"github.com/kumabox/kumabox/internal/imagestore"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vmstore"
)

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

func TestDoctorInitializesConfiguredDirectories(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
		"doctor",
		"--json",
	})

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
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"--root-dir", filepath.Join(dir, "data"),
		"network", "ls", "--json",
	})

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
	store := vmstore.New(rootDir)
	rec, err := store.Create(vmstore.CreateRequest{
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
		ID:        kbnetwork.NetworkID(rec.ID, 0),
		TAP:       "kbtaptest",
		MAC:       "5a:00:00:00:00:01",
		Backend:   kbnetwork.ProviderHostTap,
		BridgeDev: "kumabox0",
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

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"--root-dir", rootDir,
		"network", "inspect", "p2-inspect", "--json",
	})
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

	create := NewRootCommand()
	create.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
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

	inspect := NewRootCommand()
	inspect.SetArgs([]string{
		"--root-dir", rootDir,
		"inspect", "p0-store", "--json",
	})
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

	ps := NewRootCommand()
	ps.SetArgs([]string{"--root-dir", rootDir, "ps", "--json"})
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

func TestCreateRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	args := []string{
		"--root-dir", filepath.Join(dir, "data"),
		"--run-dir", filepath.Join(dir, "run"),
		"--log-dir", filepath.Join(dir, "log"),
		"create",
		"--name", "duplicate",
		"--root-disk", "fixtures/base.qcow2",
		"--kernel", "fixtures/vmlinuz",
		"--initrd", "fixtures/initrd.img",
	}

	first := NewRootCommand()
	first.SetArgs(args)
	if err := first.Execute(); err != nil {
		t.Fatal(err)
	}

	second := NewRootCommand()
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

	create := NewRootCommand()
	create.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
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
			Path:   rootDisk,
			Format: "qcow2",
		},
		Boot: imagestore.Boot{Mode: "uefi", Firmware: firmware},
		OS:   imagestore.OS{Family: "ubuntu", Profile: "ubuntu-cloudimg"},
	})
	if err != nil {
		t.Fatal(err)
	}

	create := NewRootCommand()
	create.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
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
	if created.RootDisk != rootDisk || created.Firmware != firmware {
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
	if len(rendered.Disks) == 0 || rendered.Disks[0].Path != rootDisk {
		t.Fatalf("rendered disks = %+v", rendered.Disks)
	}
}

func TestLogsCommandTailsVMLogs(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")

	create := NewRootCommand()
	create.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
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

	logs := NewRootCommand()
	logs.SetArgs([]string{"--root-dir", rootDir, "logs", "loggy", "--tail", "1"})
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

	vmmLogs := NewRootCommand()
	vmmLogs.SetArgs([]string{"--root-dir", rootDir, "logs", "loggy", "--source", "vmm", "--tail", "1"})
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

	create := NewRootCommand()
	create.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
		"create",
		"--name", "delete-cli",
		"--root-disk", rootDisk,
		"--kernel", "fixtures/vmlinuz",
		"--initrd", "fixtures/initrd.img",
	})
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	del := NewRootCommand()
	del.SetArgs([]string{"--root-dir", rootDir, "delete", "delete-cli"})
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

	ps := NewRootCommand()
	ps.SetArgs([]string{"--root-dir", rootDir, "ps", "--json"})
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

func TestGCDryRunCommandReportsCandidates(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	runDir := filepath.Join(dir, "run")
	logDir := filepath.Join(dir, "log")
	orphan := filepath.Join(runDir, "vms", "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := NewRootCommand()
	cmd.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
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

func TestImageListAndInspectCommands(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")

	listEmpty := NewRootCommand()
	listEmpty.SetArgs([]string{"--root-dir", rootDir, "image", "ls", "--json"})
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

	inspect := NewRootCommand()
	inspect.SetArgs([]string{"--root-dir", rootDir, "image", "inspect", "ubuntu", "--json"})
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
	image, err := imagestore.New(rootDir).Create(imagestore.CreateRequest{
		Name:   "ubuntu",
		Source: imagestore.Source{Type: "test", URI: "fixtures/ubuntu.img"},
		RootDisk: imagestore.RootDisk{
			Path:   filepath.Join(rootDir, "cloudimg", "img_test", "base.qcow2"),
			Format: "qcow2",
		},
		Boot: imagestore.Boot{Mode: "uefi", Firmware: "CLOUDHV.fd"},
		OS:   imagestore.OS{Family: "ubuntu", Profile: "ubuntu-cloudimg"},
	})
	if err != nil {
		t.Fatal(err)
	}

	create := NewRootCommand()
	create.SetArgs([]string{
		"--root-dir", rootDir,
		"--run-dir", runDir,
		"--log-dir", logDir,
		"create", "ubuntu",
		"--name", "ref",
	})
	if err := create.Execute(); err != nil {
		t.Fatal(err)
	}

	rm := NewRootCommand()
	rm.SetArgs([]string{"--root-dir", rootDir, "image", "rm", "ubuntu"})
	if err := rm.Execute(); !errors.Is(err, imagestore.ErrImageInUse) {
		t.Fatalf("expected ErrImageInUse, got %v", err)
	}
	if _, err := imagestore.New(rootDir).Inspect(image.ID); err != nil {
		t.Fatalf("referenced image should remain: %v", err)
	}

	del := NewRootCommand()
	del.SetArgs([]string{"--root-dir", rootDir, "--run-dir", runDir, "--log-dir", logDir, "delete", "ref"})
	if err := del.Execute(); err != nil {
		t.Fatal(err)
	}

	rm = NewRootCommand()
	rm.SetArgs([]string{"--root-dir", rootDir, "image", "rm", "ubuntu"})
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

	importCmd := NewRootCommand()
	importCmd.SetArgs([]string{
		"--root-dir", rootDir,
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

	inspect := NewRootCommand()
	inspect.SetArgs([]string{"--root-dir", rootDir, "image", "inspect", "ubuntu", "--json"})
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

	pullCmd := NewRootCommand()
	pullCmd.SetArgs([]string{
		"--root-dir", rootDir,
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

	inspect := NewRootCommand()
	inspect.SetArgs([]string{"--root-dir", rootDir, "image", "inspect", "ubuntu-pull", "--json"})
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
