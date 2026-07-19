package vmstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateInspectList(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	rec, err := store.Create(CreateRequest{
		Name:     "p0-store",
		RootDisk: "fixtures/base.qcow2",
		Kernel:   "fixtures/vmlinuz",
		Initrd:   "fixtures/initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == "" {
		t.Fatal("expected VM ID")
	}
	if rec.State != StateCreated {
		t.Fatalf("state = %s", rec.State)
	}
	if !filepath.IsAbs(rec.RootDisk) {
		t.Fatalf("root disk is not absolute: %s", rec.RootDisk)
	}
	if rec.Config != filepath.Join(rec.RunDir, "cloud-hypervisor.json") {
		t.Fatalf("config path = %s", rec.Config)
	}

	got, err := store.Inspect("p0-store")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != rec.ID {
		t.Fatalf("inspect ID = %s, want %s", got.ID, rec.ID)
	}

	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d", len(list))
	}

	indexPath := filepath.Join(dir, "data", "backends", backendCloudHypervisor, "index.json")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRemovesRecordAndName(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))
	rec, err := store.Create(CreateRequest{
		Name:     "delete-me",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Inspect("delete-me"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inspect after delete error = %v", err)
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))
	req := CreateRequest{
		Name:     "same",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	}

	if _, err := store.Create(req); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(req); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestCreateSupportsFirmwareBoot(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	rec, err := store.Create(CreateRequest{
		Name:     "uefi",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Firmware == "" {
		t.Fatal("expected firmware path")
	}
	if rec.Kernel != "" || rec.Initrd != "" {
		t.Fatalf("unexpected direct boot fields: kernel=%q initrd=%q", rec.Kernel, rec.Initrd)
	}
	if rec.Metadata == nil {
		t.Fatal("expected NoCloud metadata")
	}
	if rec.Metadata.Type != "nocloud" {
		t.Fatalf("metadata type = %s", rec.Metadata.Type)
	}
	if rec.Metadata.CidataDir != filepath.Join(rec.RunDir, "cidata") {
		t.Fatalf("cidata dir = %s", rec.Metadata.CidataDir)
	}
	if rec.Metadata.CidataDisk != filepath.Join(rec.RunDir, "cidata.img") {
		t.Fatalf("cidata disk = %s", rec.Metadata.CidataDisk)
	}
}

func TestCreatePersistsCPUs(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	defaulted, err := store.Create(CreateRequest{
		Name:     "default-cpu",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.CPUs != 1 {
		t.Fatalf("default cpus = %d", defaulted.CPUs)
	}

	custom, err := store.Create(CreateRequest{
		Name:     "custom-cpu",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		CPUs:     4,
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if custom.CPUs != 4 {
		t.Fatalf("custom cpus = %d", custom.CPUs)
	}
}

func TestCreatePersistsMemory(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	defaulted, err := store.Create(CreateRequest{
		Name: "default-memory", RootDisk: "ubuntu.img", Firmware: "CLOUDHV.fd",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.MemoryBytes != 512<<20 {
		t.Fatalf("default memory = %d", defaulted.MemoryBytes)
	}

	custom, err := store.Create(CreateRequest{
		Name: "custom-memory", RootDisk: "ubuntu.img", Firmware: "CLOUDHV.fd", MemoryBytes: 2 << 30,
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if custom.MemoryBytes != 2<<30 {
		t.Fatalf("custom memory = %d", custom.MemoryBytes)
	}
}

func TestCreatePersistsImageRef(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))
	image := &ImageRef{
		ID:       "img_123",
		Name:     "ubuntu",
		RootDisk: filepath.Join(dir, "images", "base.qcow2"),
		BootMode: "uefi",
	}

	rec, err := store.Create(CreateRequest{
		Name:     "from-image",
		RootDisk: image.RootDisk,
		Firmware: "CLOUDHV.fd",
		Image:    image,
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Image == nil {
		t.Fatal("expected image ref")
	}
	if rec.Image.ID != image.ID || rec.Image.Name != image.Name || rec.Image.RootDisk != image.RootDisk || rec.Image.BootMode != image.BootMode {
		t.Fatalf("image ref = %+v", rec.Image)
	}

	image.Name = "mutated"
	inspected, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Image.Name != "ubuntu" {
		t.Fatalf("image ref was not defensively copied: %+v", inspected.Image)
	}
}

func TestCreatePersistsNetworkAttachments(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	rec, err := store.Create(CreateRequest{
		Name:     "multi-net",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		Networks: []string{"cni:front", "cni:back"},
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Network != "multi" {
		t.Fatalf("legacy network = %s", rec.Network)
	}
	if len(rec.Networks) != 2 || rec.Networks[0] != "cni:front" || rec.Networks[1] != "cni:back" {
		t.Fatalf("networks = %#v", rec.Networks)
	}

	inspected, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.Networks) != 2 || inspected.Networks[0] != "cni:front" || inspected.Networks[1] != "cni:back" {
		t.Fatalf("inspected networks = %#v", inspected.Networks)
	}
}

func TestCreateRejectsNoneWithOtherNetworks(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	_, err := store.Create(CreateRequest{
		Name:     "bad-net",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		Networks: []string{"none", "default"},
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err == nil {
		t.Fatal("expected mixed none network error")
	}
}

func TestCreateRejectsMixedNetworkProviderFamilies(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	_, err := store.Create(CreateRequest{
		Name:     "mixed-provider-net",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		Networks: []string{"default", "cni:isolated"},
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err == nil {
		t.Fatal("expected mixed provider family error")
	}
}

func TestMarkRunningMarksFirmwareVMFirstBooted(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	rec, err := store.Create(CreateRequest{
		Name:     "uefi",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.FirstBooted {
		t.Fatal("new VM should not be marked first-booted")
	}

	running, err := store.MarkRunning(rec.ID, 1234, filepath.Join(rec.RunDir, "ch.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if !running.FirstBooted {
		t.Fatal("firmware VM should be marked first-booted after successful start")
	}
}

func TestMarkPerformancePersistsDefensivePhaseMetrics(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))
	rec, err := store.Create(CreateRequest{
		Name:     "performance",
		RootDisk: "base.qcow2", Kernel: "vmlinuz", Initrd: "initrd.img",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	phase := time.Now().UTC()
	metrics := PerformanceMetrics{
		Operation: "run", CommandStartedAt: phase,
		ImageResolvedAt: &phase, ReadyDurationMs: 42,
	}
	updated, err := store.MarkPerformance(rec.ID, metrics)
	if err != nil {
		t.Fatal(err)
	}
	*updated.Performance.ImageResolvedAt = updated.Performance.ImageResolvedAt.Add(time.Hour)
	inspected, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Performance == nil || inspected.Performance.ReadyDurationMs != 42 {
		t.Fatalf("performance = %+v", inspected.Performance)
	}
	if inspected.Performance.ImageResolvedAt.Equal(*updated.Performance.ImageResolvedAt) {
		t.Fatal("inspect returned mutable performance timestamp")
	}
}

func TestMarkRestoredMarksFirmwareVMFirstBooted(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	rec, err := store.Create(CreateRequest{
		Name:     "restored-uefi",
		RootDisk: "ubuntu.img",
		Firmware: "CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginRestore(rec.ID, "snap_test", "copy"); err != nil {
		t.Fatal(err)
	}
	restored, err := store.MarkRestored(rec.ID, 1234, filepath.Join(rec.RunDir, "ch.sock"), 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.FirstBooted {
		t.Fatal("restored firmware VM should not regenerate first-boot metadata")
	}
}

func TestMarkRestoredPinsDelayedMemoryUntilStop(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))
	rec, err := store.Create(CreateRequest{
		Name: "delayed", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd", RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginRestore(rec.ID, "snap_delayed", "mmap"); err != nil {
		t.Fatal(err)
	}
	restored, err := store.MarkRestored(rec.ID, 1234, filepath.Join(rec.RunDir, "ch.sock"), 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if restored.SnapshotDependency == nil || restored.SnapshotDependency.SnapshotID != "snap_delayed" {
		t.Fatalf("snapshot dependency = %+v", restored.SnapshotDependency)
	}
	if restored.LastRestore == nil || restored.LastRestore.Mode != "mmap" || restored.LastRestore.DurationMs != 250 {
		t.Fatalf("last restore = %+v", restored.LastRestore)
	}
	stopped, err := store.MarkStopped(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.SnapshotDependency != nil {
		t.Fatalf("stopped VM retained dependency = %+v", stopped.SnapshotDependency)
	}
}

func TestMarkRestoredPersistsPhaseMetrics(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))
	rec, err := store.Create(CreateRequest{
		Name: "timed-restore", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginRestore(rec.ID, "snap_timed", "copy"); err != nil {
		t.Fatal(err)
	}
	restored, err := store.MarkRestoredWithMetrics(rec.ID, 1234, filepath.Join(rec.RunDir, "ch.sock"), time.Second, &RestoreResult{
		NativeStageDurationMs: 11, DiskStageDurationMs: 22, DiskCommitDurationMs: 3,
		BackendRestoreDurationMs: 44, IdentityDurationMs: 55, ReadinessDurationMs: 66,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.LastRestore == nil || restored.LastRestore.DiskStageDurationMs != 22 || restored.LastRestore.ReadinessDurationMs != 66 {
		t.Fatalf("last restore metrics = %+v", restored.LastRestore)
	}
	persisted, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.LastRestore == nil || persisted.LastRestore.BackendRestoreDurationMs != 44 {
		t.Fatalf("persisted restore metrics = %+v", persisted.LastRestore)
	}
}

func TestCreateRejectsMixedFirmwareAndDirectBoot(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "data"))

	_, err := store.Create(CreateRequest{
		Name:     "mixed",
		RootDisk: "ubuntu.img",
		Kernel:   "vmlinuz",
		Firmware: "CLOUDHV.fd",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err == nil {
		t.Fatal("expected mixed boot error")
	}
}

func TestResolveByIDPrefix(t *testing.T) {
	idx := &vmIndex{
		VMs: map[string]*VMRecord{
			"kb_abcdef": {ID: "kb_abcdef"},
		},
		Names: map[string]string{},
	}

	id, err := idx.resolve("kb_abc")
	if err != nil {
		t.Fatal(err)
	}
	if id != "kb_abcdef" {
		t.Fatalf("id = %s", id)
	}
}
