package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestCloneNativeSnapshotCreatesIndependentRunningVM(t *testing.T) {
	rt, store, source, ready := newNativeCloneRuntime(t)
	rt.guestReadiness = func(context.Context, string) error { return nil }
	originalIdentity := configureGuestIdentity
	configureGuestIdentity = func(_ context.Context, socket string, rec *vmstore.VMRecord) error {
		if rec.ID == source.ID || rec.Name != "clone" {
			t.Fatalf("clone identity = %s/%s", rec.ID, rec.Name)
		}
		if socket != rec.VsockSocket {
			t.Fatalf("identity socket = %q, want %q", socket, rec.VsockSocket)
		}
		return nil
	}
	defer func() { configureGuestIdentity = originalIdentity }()

	state := vmstore.ObservedStateRunning
	rt.backend = backendFake{
		render: func(*vmstore.VMRecord) error { return nil },
		observe: func(*vmstore.VMRecord) vmstore.Observation {
			return vmstore.Observation{State: state, CheckedAt: time.Now().UTC()}
		},
		clone: func(_ context.Context, rec *vmstore.VMRecord, nativeDir, mode string) (*backend.StartResult, error) {
			if rec.ID == source.ID || rec.Restore == nil || mode != "copy" {
				t.Fatalf("clone backend record = %+v", rec)
			}
			memoryPath := filepath.Join(nativeDir, "memory-range-0")
			memoryInfo, err := os.Stat(memoryPath)
			if err != nil {
				t.Fatal(err)
			}
			sourceInfo, err := os.Stat(filepath.Join(ready.DataDir, snapshot.NativePayloadDir, "memory-range-0"))
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(sourceInfo, memoryInfo) {
				t.Fatal("copy clone copied native memory before Cloud Hypervisor restore")
			}
			return &backend.StartResult{PID: 9876, APISocket: filepath.Join(rec.RunDir, "ch.sock")}, nil
		},
	}

	cloned, err := rt.CloneNativeSnapshot(context.Background(), ready.ID, NativeCloneOptions{Name: "clone", Networks: []string{"none"}})
	if err != nil {
		t.Fatal(err)
	}
	if cloned.ID == source.ID || cloned.State != vmstore.StateRunning || cloned.PID != 9876 || cloned.Restore != nil {
		t.Fatalf("cloned record = %+v", cloned)
	}
	if cloned.StorageConfigs[1].Path == source.StorageConfigs[1].Path {
		t.Fatal("clone reused source writable disk")
	}
	content, err := os.ReadFile(cloned.StorageConfigs[1].Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "source-cow" {
		t.Fatalf("clone disk = %q", content)
	}
	persistedSource, err := store.Inspect(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedSource.State != vmstore.StateRunning || persistedSource.PID != source.PID {
		t.Fatalf("source changed = %+v", persistedSource)
	}
	if cloned.LastRestore == nil || cloned.LastRestore.DiskStageDurationMs < 0 || cloned.LastRestore.IdentityDurationMs < 0 || cloned.LastRestore.ReadinessDurationMs < 0 {
		t.Fatalf("clone metrics = %+v", cloned.LastRestore)
	}
	if _, err := os.Stat(filepath.Join(cloned.RunDir, ".restore-staging")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy clone retained native staging: %v", err)
	}
}

func TestCloneNativeSnapshotPreservesFailedBackend(t *testing.T) {
	rt, store, _, ready := newNativeCloneRuntime(t)
	cloneErr := errors.New("injected clone failure")
	rt.backend = backendFake{
		render: func(*vmstore.VMRecord) error { return nil },
		clone: func(context.Context, *vmstore.VMRecord, string, string) (*backend.StartResult, error) {
			return nil, cloneErr
		},
	}
	if _, err := rt.CloneNativeSnapshot(context.Background(), ready.ID, NativeCloneOptions{Name: "failed-clone", Networks: []string{"none"}}); !errors.Is(err, cloneErr) {
		t.Fatalf("clone error = %v", err)
	}
	preserved, err := store.Inspect("failed-clone")
	if err != nil {
		t.Fatalf("inspect failed clone: %v", err)
	}
	if preserved.State != vmstore.StateError || preserved.Restore == nil || preserved.Restore.State != "failed" {
		t.Fatalf("failed clone = %+v", preserved)
	}
	diagnosticRoot := filepath.Join(store.RootDir(), "diagnostics", "native-clone")
	entries, err := os.ReadDir(diagnosticRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("native clone diagnostics = %v, entries=%v", err, entries)
	}
	diagnosticDir := filepath.Join(diagnosticRoot, entries[0].Name())
	for _, name := range []string{"failure.txt", "vm.json", "snapshot.json"} {
		if _, statErr := os.Stat(filepath.Join(diagnosticDir, name)); statErr != nil {
			t.Fatalf("diagnostic %s: %v", name, statErr)
		}
	}
	failure, err := os.ReadFile(filepath.Join(diagnosticDir, "failure.txt"))
	if err != nil || !strings.Contains(string(failure), cloneErr.Error()) {
		t.Fatalf("diagnostic failure = %q, err=%v", failure, err)
	}
}

func TestCloneNativeSnapshotCapturesDiagnosticsBeforeStoppingBackend(t *testing.T) {
	rt, store, _, ready := newNativeCloneRuntime(t)
	readinessErr := errors.New("injected readiness failure")
	rt.guestReadiness = func(context.Context, string) error { return readinessErr }
	originalIdentity := configureGuestIdentity
	configureGuestIdentity = func(context.Context, string, *vmstore.VMRecord) error { return nil }
	defer func() { configureGuestIdentity = originalIdentity }()

	rt.backend = backendFake{
		render: func(rec *vmstore.VMRecord) error {
			if err := os.MkdirAll(filepath.Dir(rec.Config), 0o700); err != nil {
				return err
			}
			if err := os.MkdirAll(rec.LogDir, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(rec.Config, []byte(`{"live":true}`), 0o600); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(rec.LogDir, "cloud-hypervisor.stderr.log"), []byte("live failure\n"), 0o600)
		},
		clone: func(_ context.Context, rec *vmstore.VMRecord, _ string, _ string) (*backend.StartResult, error) {
			return &backend.StartResult{PID: 9876, APISocket: filepath.Join(rec.RunDir, "ch.sock")}, nil
		},
		stop: func(rec *vmstore.VMRecord, _ backend.StopOptions) (*backend.StopResult, error) {
			if err := os.RemoveAll(rec.RunDir); err != nil {
				return nil, err
			}
			if err := os.RemoveAll(rec.LogDir); err != nil {
				return nil, err
			}
			return &backend.StopResult{}, nil
		},
	}

	if _, err := rt.CloneNativeSnapshot(context.Background(), ready.ID, NativeCloneOptions{
		Name: "readiness-failure", Networks: []string{"none"},
	}); !errors.Is(err, readinessErr) {
		t.Fatalf("clone error = %v", err)
	}
	preserved, err := store.Inspect("readiness-failure")
	if err != nil {
		t.Fatalf("inspect failed clone: %v", err)
	}
	diagnosticDir := filepath.Join(store.RootDir(), "diagnostics", "native-clone", preserved.ID)
	config, err := os.ReadFile(filepath.Join(diagnosticDir, "cloud-hypervisor.json"))
	if err != nil || string(config) != `{"live":true}` {
		t.Fatalf("preserved config = %q, err=%v", config, err)
	}
	stderr, err := os.ReadFile(filepath.Join(diagnosticDir, "cloud-hypervisor.stderr.log"))
	if err != nil || string(stderr) != "live failure\n" {
		t.Fatalf("preserved stderr = %q, err=%v", stderr, err)
	}
}

func TestCloneNativeSnapshotPinsDelayedMemoryPayload(t *testing.T) {
	tests := []struct {
		name string
		mode RestoreMode
	}{
		{name: "ondemand", mode: RestoreModeOnDemand},
		{name: "mmap", mode: RestoreModeMmap},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rt, store, _, ready := newNativeCloneRuntime(t)
			rt.guestReadiness = func(context.Context, string) error { return nil }
			originalIdentity := configureGuestIdentity
			configureGuestIdentity = func(context.Context, string, *vmstore.VMRecord) error { return nil }
			defer func() { configureGuestIdentity = originalIdentity }()

			rt.backend = backendFake{
				nativeHost: func(context.Context, *vmstore.VMRecord) (backend.NativeHost, error) {
					return backend.NativeHost{
						BackendName: "cloud-hypervisor", BackendVersion: "test", SnapshotFormat: "cloud-hypervisor-native-v1",
						Architecture: "test", CPUVendor: "test", RestoreModes: []string{"copy", string(test.mode)},
					}, nil
				},
				render: func(*vmstore.VMRecord) error { return nil },
				clone: func(_ context.Context, rec *vmstore.VMRecord, _ string, mode string) (*backend.StartResult, error) {
					if mode != string(test.mode) {
						t.Fatalf("backend mode = %q, want %q", mode, test.mode)
					}
					return &backend.StartResult{PID: 9876, APISocket: filepath.Join(rec.RunDir, "ch.sock")}, nil
				},
				observe: func(*vmstore.VMRecord) vmstore.Observation {
					return vmstore.Observation{State: vmstore.ObservedStateRunning, CheckedAt: time.Now().UTC()}
				},
			}

			cloned, err := rt.CloneNativeSnapshot(context.Background(), ready.ID, NativeCloneOptions{
				Name: test.name + "-clone", Networks: []string{"none"}, Mode: test.mode,
			})
			if err != nil {
				t.Fatal(err)
			}
			if cloned.SnapshotDependency == nil || cloned.SnapshotDependency.SnapshotID != ready.ID || cloned.SnapshotDependency.Mode != string(test.mode) {
				t.Fatalf("snapshot dependency = %+v", cloned.SnapshotDependency)
			}
			if _, err := os.Stat(filepath.Join(cloned.RunDir, ".restore-staging", snapshot.NativePayloadDir, "memory-range-0")); err != nil {
				t.Fatalf("%s clone discarded delayed memory payload: %v", test.mode, err)
			}
			if _, err := snapshot.NewStore(store.RootDir()).Remove(ready.ID); !errors.Is(err, snapshot.ErrInUse) {
				t.Fatalf("remove %s snapshot error = %v", test.mode, err)
			}
		})
	}
}

func newNativeCloneRuntime(t *testing.T) (*Runtime, *vmstore.Store, *vmstore.VMRecord, *snapshot.Record) {
	t.Helper()
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "data")
	kernel := filepath.Join(dir, "vmlinuz")
	initrd := filepath.Join(dir, "initrd")
	layer := filepath.Join(dir, "layer.erofs")
	for path, content := range map[string]string{kernel: "kernel", initrd: "initrd", layer: "layer"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const manifestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const layerDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	image, err := imagestore.New(rootDir).Create(imagestore.CreateRequest{
		Name: "clone-image", Boot: imagestore.Boot{Mode: "direct", Kernel: kernel, Initrd: initrd, Cmdline: "console=ttyS0"},
		OCI: &imagestore.OCI{DigestRef: "example.invalid/image@" + manifestDigest, Layers: []imagestore.OCILayer{{
			Index: 0, Digest: layerDigest, EROFS: &imagestore.EROFSLayer{Path: layer, Filesystem: "erofs", SizeBytes: 5, SourceLayer: layerDigest},
		}}, BuiltAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := vmstore.New(rootDir)
	source, err := store.Create(vmstore.CreateRequest{
		Name: "source", Kernel: kernel, Initrd: initrd, Network: "none",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
		Image: &vmstore.ImageRef{ID: image.ID, Name: image.Name, BootMode: "direct", Digest: manifestDigest, LayerDigests: []string{layerDigest}},
		StorageConfigs: []vmstore.StorageConfig{
			{ID: "layer0", Role: vmstore.StorageRoleLayer, Path: layer, Readonly: true, Format: "raw", Filesystem: "erofs", Serial: "kumabox-layer0", SourceLayer: layerDigest, VirtualSizeBytes: 5},
			{ID: "cow", Role: vmstore.StorageRoleCOW, Format: "raw", Filesystem: "ext4", Serial: "kumabox-cow", VirtualSizeBytes: 10, Base: &vmstore.StorageBase{Family: "oci", ImageID: image.ID, Digest: manifestDigest, LayerDigests: []string{layerDigest}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(source.StorageConfigs[1].Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.StorageConfigs[1].Path, []byte("source-cow"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err = store.MarkStarted(source.ID, 1234, filepath.Join(source.RunDir, "ch.sock"))
	if err != nil {
		t.Fatal(err)
	}

	snapshotStore := snapshot.NewStore(rootDir)
	build, err := snapshotStore.Reserve(context.Background(), "clone-source")
	if err != nil {
		t.Fatal(err)
	}
	staging := build.Record().StagingDir
	nativeDir := filepath.Join(staging, "native")
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configJSON := fmt.Sprintf(`{"cpus":{"boot_vcpus":1},"memory":{"size":536870912},"disks":[{"path":%q,"readonly":true},{"path":%q,"readonly":false}],"net":[],"vsock":{}}`, layer, source.StorageConfigs[1].Path)
	for name, content := range map[string]string{"config.json": configJSON, "state.json": "{}", "memory-range-0": "memory"} {
		if err := os.WriteFile(filepath.Join(nativeDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	disks, _, err := snapshot.CaptureWritableDisks(context.Background(), staging, source)
	if err != nil {
		t.Fatal(err)
	}
	_, size, err := snapshot.WriteNativeManifest(context.Background(), build, source, disks, backend.NativeHost{
		BackendName: "cloud-hypervisor", BackendVersion: "test", SnapshotFormat: "cloud-hypervisor-native-v1", Architecture: "test", CPUVendor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := build.Finalize(size)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime.RootDir = rootDir
	cfg.Runtime.RunDir = filepath.Join(dir, "run")
	cfg.Runtime.LogDir = filepath.Join(dir, "log")
	rt := NewWithBackend(store, backendFake{})
	rt.cfg = cfg
	return rt, store, source, ready
}
