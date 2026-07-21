package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/snapshot"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestCreateRunningSnapshotCapturesOnePauseWindow(t *testing.T) {
	t.Parallel()

	rt, store, rec, sourceDisk := newRunningSnapshotRuntime(t)
	backendState := vmstore.ObservedStateRunning
	steps := make([]string, 0, 3)
	rt.backend = backendFake{
		observe: func(*vmstore.VMRecord) vmstore.Observation {
			return vmstore.Observation{State: backendState, CheckedAt: time.Now().UTC()}
		},
		pause: func(context.Context, *vmstore.VMRecord) error {
			steps = append(steps, "pause")
			backendState = vmstore.ObservedStatePaused
			return nil
		},
		snapshot: func(_ context.Context, _ *vmstore.VMRecord, destination string) error {
			steps = append(steps, "snapshot")
			for name, content := range map[string]string{
				"config.json": fmt.Sprintf(`{"cpus":{"boot_vcpus":1},"memory":{"size":536870912},"disks":[{"path":%q,"readonly":false}],"vsock":{}}`, rec.StorageConfigs[0].Path),
				"state.json":  "{}", "memory-range-0": "memory",
			} {
				if err := os.WriteFile(filepath.Join(destination, name), []byte(content), 0o600); err != nil {
					return err
				}
			}
			return nil
		},
		resume: func(context.Context, *vmstore.VMRecord) error {
			steps = append(steps, "resume")
			backendState = vmstore.ObservedStateRunning
			return nil
		},
	}

	ready, err := rt.CreateRunningSnapshot(context.Background(), rec.ID, "running")
	if err != nil {
		t.Fatal(err)
	}
	if got := steps; len(got) != 3 || got[0] != "pause" || got[1] != "snapshot" || got[2] != "resume" {
		t.Fatalf("capture steps = %v", got)
	}
	manifest, err := snapshot.NewStore(store.RootDir()).LoadManifest(context.Background(), ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != "kumabox.snapshot.v2" || manifest.Type != "native" || manifest.Consistency != "crash" || manifest.Native == nil || len(manifest.Native.Files) != 3 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Backend == nil || manifest.Machine == nil || manifest.Machine.MemoryBytes != 512<<20 || manifest.Native.Files[0].SHA256 != "" {
		t.Fatalf("compatibility metadata = %+v", manifest)
	}
	if ready.Performance == nil || ready.Performance.TotalDurationMs < ready.Performance.PauseDurationMs {
		t.Fatalf("capture performance = %+v", ready.Performance)
	}
	if _, err := os.Stat(filepath.Join(ready.DataDir, "disks", "cow.raw")); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(sourceDisk); err != nil || string(raw) != "writable" {
		t.Fatalf("source disk changed: %q, %v", raw, err)
	}
}

func TestCreateRunningSnapshotResumesAfterCaptureFailure(t *testing.T) {
	t.Parallel()

	rt, store, rec, _ := newRunningSnapshotRuntime(t)
	backendState := vmstore.ObservedStateRunning
	resumed := false
	rt.backend = backendFake{
		observe: func(*vmstore.VMRecord) vmstore.Observation {
			return vmstore.Observation{State: backendState, CheckedAt: time.Now().UTC()}
		},
		pause: func(context.Context, *vmstore.VMRecord) error {
			backendState = vmstore.ObservedStatePaused
			return nil
		},
		snapshot: func(context.Context, *vmstore.VMRecord, string) error {
			return errors.New("injected capture failure")
		},
		resume: func(context.Context, *vmstore.VMRecord) error {
			resumed = true
			backendState = vmstore.ObservedStateRunning
			return nil
		},
	}

	if _, err := rt.CreateRunningSnapshot(context.Background(), rec.ID, "failed"); err == nil {
		t.Fatal("expected capture failure")
	}
	if !resumed {
		t.Fatal("VM was not resumed after capture failure")
	}
	if records, err := snapshot.NewStore(store.RootDir()).Scan(); err != nil || len(records) != 0 {
		t.Fatalf("failed capture leaked snapshot records: %+v, %v", records, err)
	}
}

func writeNativeSnapshotFixture(destination string, rec *vmstore.VMRecord) error {
	for name, content := range map[string]string{
		"config.json": fmt.Sprintf(`{"cpus":{"boot_vcpus":1},"memory":{"size":536870912},"disks":[{"path":%q,"readonly":false}],"vsock":{}}`, rec.StorageConfigs[0].Path),
		"state.json":  "{}", "memory-range-0": "memory",
	} {
		if err := os.WriteFile(filepath.Join(destination, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func newRunningSnapshotRuntime(t *testing.T) (*Runtime, *vmstore.Store, *vmstore.VMRecord, string) {
	t.Helper()
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	kernel := filepath.Join(dir, "vmlinuz")
	initrd := filepath.Join(dir, "initrd")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrd, []byte("initrd"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Create(vmstore.CreateRequest{
		Name: "source", Kernel: kernel, Initrd: initrd,
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"), Network: "none",
		StorageConfigs: []vmstore.StorageConfig{{
			ID: "cow", Role: vmstore.StorageRoleData, Format: "raw", Filesystem: "ext4",
			VirtualSizeBytes: int64(len("writable")),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceDisk := rec.StorageConfigs[0].Path
	if err := os.MkdirAll(filepath.Dir(sourceDisk), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceDisk, []byte("writable"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err = store.MarkRunning(rec.ID, 1234, filepath.Join(rec.RunDir, "ch.sock"))
	if err != nil {
		t.Fatal(err)
	}
	return NewWithBackend(store, backendFake{}), store, rec, sourceDisk
}
