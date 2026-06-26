package runtime

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

type rendererFunc func(*vmstore.VMRecord) error

func (fn rendererFunc) RenderConfig(rec *vmstore.VMRecord) error {
	return fn(rec)
}

type starterFunc func(string) (*backend.StartResult, error)

func (fn starterFunc) StartConfig(path string) (*backend.StartResult, error) {
	return fn(path)
}

func TestCreateVMRollsBackRecordOnRenderFailure(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	renderErr := errors.New("render failed")
	rt := NewWithBackend(store, rendererFunc(func(*vmstore.VMRecord) error {
		return renderErr
	}), starterFunc(nil))

	_, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "rollback",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if !errors.Is(err, renderErr) {
		t.Fatalf("error = %v, want %v", err, renderErr)
	}

	if _, err := store.Inspect("rollback"); !errors.Is(err, vmstore.ErrNotFound) {
		t.Fatalf("inspect after rollback error = %v", err)
	}
}

func TestStartVMMarksRunning(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	rt := NewWithBackend(
		store,
		rendererFunc(func(*vmstore.VMRecord) error { return nil }),
		starterFunc(func(string) (*backend.StartResult, error) {
			return &backend.StartResult{PID: 1234, APISocket: "/tmp/ch.sock"}, nil
		}),
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "start-me",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}

	started, err := rt.StartVM(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != vmstore.StateRunning {
		t.Fatalf("state = %s", started.State)
	}
	if started.PID != 1234 || started.APISocket != "/tmp/ch.sock" {
		t.Fatalf("runtime fields = pid %d socket %s", started.PID, started.APISocket)
	}
}

func TestStartVMMarksErrorOnStartFailure(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	startErr := errors.New("start failed")
	rt := NewWithBackend(
		store,
		rendererFunc(func(*vmstore.VMRecord) error { return nil }),
		starterFunc(func(string) (*backend.StartResult, error) { return nil, startErr }),
	)

	rec, err := rt.CreateVM(vmstore.CreateRequest{
		Name:     "fail-me",
		RootDisk: "base.qcow2",
		Kernel:   "vmlinuz",
		Initrd:   "initrd.img",
		RunDir:   filepath.Join(dir, "run"),
		LogDir:   filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rt.StartVM(rec.ID); !errors.Is(err, startErr) {
		t.Fatalf("start error = %v", err)
	}
	updated, err := store.Inspect(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != vmstore.StateError || updated.Error == "" {
		t.Fatalf("updated record = %+v", updated)
	}
}
