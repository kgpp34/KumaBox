package core

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

type fakeFileShareRuntime struct {
	*fakeRuntime
	shares []types.AttachedFileShare
}

func (f *fakeFileShareRuntime) AttachedFileShares(context.Context, vmm.Process) ([]types.AttachedFileShare, error) {
	return f.shares, nil
}

func (f *fakeFileShareRuntime) AddFileShare(_ context.Context, _ vmm.Process, share types.FileShare) error {
	f.shares = append(f.shares, types.AttachedFileShare{ID: "kumabox-fs-" + share.Tag, Tag: share.Tag, Socket: share.Socket})
	return nil
}

func (f *fakeFileShareRuntime) RemoveFileShare(_ context.Context, _ vmm.Process, tag string) error {
	f.shares = nil
	return nil
}

func TestFileShareRequiresSharedMemoryAndStaysRuntimeOnly(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "kbfs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "virtiofsd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	for _, shared := range []bool{false, true} {
		service, _ := newTestSandboxService(t, nil)
		runtime := &fakeFileShareRuntime{fakeRuntime: testRuntime(t, service)}
		service.dependencies.runtimes, err = vmm.NewRegistry(runtime)
		if err != nil {
			t.Fatal(err)
		}
		record, err := service.Run(t.Context(), CreateSandboxRequest{
			ImageReference: "demo",
			Config: types.SandboxConfig{
				Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory,
				SharedMemory: shared, Storage: types.DefaultSandboxStorage,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		runtime.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{
			PID: 42, StartTicks: 10, BootID: "boot", SandboxID: record.ID,
			Generation: record.Generation - 1, Binary: "cloud-hypervisor", APISocket: "/run/kumabox/api.sock",
		}}
		devices, err := service.AttachFileShare(t.Context(), "box", types.FileShare{Socket: socket, Tag: "data"})
		if !shared {
			if err == nil || len(runtime.shares) != 0 {
				t.Fatalf("private-memory VM accepted a file share: %+v, %v", devices, err)
			}
			continue
		}
		if err != nil || len(devices.FS) != 1 || devices.FS[0].Tag != "data" {
			t.Fatalf("shared-memory attach = %+v, %v", devices, err)
		}
		devices, err = service.DetachFileShare(t.Context(), "box", "data")
		if err != nil || len(devices.FS) != 0 {
			t.Fatalf("runtime-only detach = %+v, %v", devices, err)
		}
	}
}

func TestResolveExternalDiskRejectsManagedPathsAfterSymlinks(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "data")
	external := filepath.Join(root, "external")
	if err := os.Mkdir(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(managed, "owned.raw")
	outside := filepath.Join(external, "volume.raw")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("raw"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(external, "alias.raw")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	roots := storage.Roots{Data: managed, Run: filepath.Join(root, "run"), Log: filepath.Join(root, "log")}
	if _, err := resolveExternalDisk(inside, roots); err == nil {
		t.Fatal("managed disk was accepted")
	}
	if _, err := resolveExternalDisk(alias, roots); err == nil {
		t.Fatal("symlink to managed disk was accepted")
	}
	expected, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := resolveExternalDisk(outside, roots); err != nil || got != expected {
		t.Fatalf("external disk = %q, %v", got, err)
	}
}

func TestNormalizePCIPathRejectsOtherHostPaths(t *testing.T) {
	for input, expected := range map[string]string{
		"01:00.0":                           "/sys/bus/pci/devices/0000:01:00.0",
		"0000:03:1a.2":                      "/sys/bus/pci/devices/0000:03:1a.2",
		"/sys/bus/pci/devices/0000:03:1a.2": "/sys/bus/pci/devices/0000:03:1a.2",
	} {
		got, err := normalizePCIPath(input)
		if err != nil || got != expected {
			t.Fatalf("normalize %q = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"/etc/passwd", "/sys/bus/pci/devices/../../etc/passwd", "01:00.8"} {
		if _, err := normalizePCIPath(input); err == nil {
			t.Fatalf("unsafe PCI path %q was accepted", input)
		}
	}
}
