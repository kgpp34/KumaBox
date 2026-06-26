package runtime

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/vmstore"
)

type rendererFunc func(*vmstore.VMRecord) error

func (fn rendererFunc) RenderConfig(rec *vmstore.VMRecord) error {
	return fn(rec)
}

func TestCreateVMRollsBackRecordOnRenderFailure(t *testing.T) {
	dir := t.TempDir()
	store := vmstore.New(filepath.Join(dir, "data"))
	renderErr := errors.New("render failed")
	rt := NewWithRenderer(store, rendererFunc(func(*vmstore.VMRecord) error {
		return renderErr
	}))

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
