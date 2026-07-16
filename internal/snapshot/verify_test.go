package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestVerifyNative(t *testing.T) {
	t.Parallel()

	store, ready, target, host := buildNativeVerificationFixture(t)
	manifest, err := store.VerifyNative(context.Background(), ready.ID, NativeVerifyTarget{VM: target, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != "kumabox.snapshot.v2" || manifest.Machine.MemoryBytes != 512<<20 {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestVerifyNativeRejectsPayloadCorruption(t *testing.T) {
	t.Parallel()

	store, ready, target, host := buildNativeVerificationFixture(t)
	if err := os.WriteFile(filepath.Join(ready.DataDir, "native", "memory-range-0"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := store.VerifyNative(context.Background(), ready.ID, NativeVerifyTarget{VM: target, Host: host})
	if err == nil || !strings.Contains(err.Error(), "CHECKSUM_MISMATCH") {
		t.Fatalf("VerifyNative error = %v", err)
	}
}

func TestVerifyNativeRejectsBackendVersionMismatch(t *testing.T) {
	t.Parallel()

	store, ready, target, host := buildNativeVerificationFixture(t)
	host.BackendVersion = "99.0.0"
	_, err := store.VerifyNative(context.Background(), ready.ID, NativeVerifyTarget{VM: target, Host: host})
	if err == nil || !strings.Contains(err.Error(), "SNAPSHOT_INCOMPATIBLE: backend version") {
		t.Fatalf("VerifyNative error = %v", err)
	}
}

func buildNativeVerificationFixture(t *testing.T) (*Store, *Record, *vmstore.VMRecord, backend.NativeHost) {
	t.Helper()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinuz")
	initrd := filepath.Join(dir, "initrd")
	disk := filepath.Join(dir, "data.raw")
	for path, content := range map[string]string{kernel: "kernel", initrd: "initrd", disk: "writable"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := &vmstore.VMRecord{
		ID: "kb_source", Name: "source", Backend: "cloud-hypervisor", Kernel: kernel, Initrd: initrd,
		CPUs: 2, MemoryBytes: 512 << 20, VsockSocket: filepath.Join(dir, "vsock.uds"),
		StorageConfigs: []vmstore.StorageConfig{{
			ID: "data", Role: vmstore.StorageRoleData, Path: disk, Format: "raw", VirtualSizeBytes: int64(len("writable")),
		}},
	}
	host := backend.NativeHost{
		BackendName: "cloud-hypervisor", BackendVersion: "50.0.0", SnapshotFormat: "cloud-hypervisor-native-v1",
		Architecture: "amd64", CPUVendor: "GenuineIntel", CPUFeatures: []string{"sse4_2"},
	}
	store := NewStore(filepath.Join(dir, "data-root"))
	build, err := store.Reserve(context.Background(), "native")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = build.Abort() })
	staging := build.Record().StagingDir
	if err := os.MkdirAll(filepath.Join(staging, "native"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := `{"cpus":{"boot_vcpus":2},"memory":{"size":536870912},"disks":[{"path":"` + disk + `","readonly":false}],"vsock":{}}`
	for name, content := range map[string]string{"config.json": config, "state.json": "{}", "memory-range-0": "memory"} {
		if err := os.WriteFile(filepath.Join(staging, "native", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(staging, "disks"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(staging, "disks", "data.raw")
	if err := os.WriteFile(payload, []byte("writable"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := hashFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	disks := []DiskManifest{{
		ID: "data", Role: "data", Path: "disks/data.raw", Format: "raw",
		VirtualSizeBytes: int64(len("writable")), AllocatedSizeBytes: int64(len("writable")), SHA256: digest, CopyStrategy: "stream",
	}}
	_, size, err := WriteNativeManifest(context.Background(), build, target, disks, host, "crash")
	if err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(size)
	if err != nil {
		t.Fatal(err)
	}
	return store, ready, target, host
}
