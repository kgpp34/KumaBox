package disk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestQEMUImgEnsureOverlayCreatesAndValidatesBacking(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	overlay := filepath.Join(dir, "vm", "root.overlay.qcow2")
	if err := os.WriteFile(base, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	qemuImg := NewQEMUImg(fakeQEMUImg(t, dir, base))
	spec := OverlaySpec{Path: overlay, BasePath: base, BaseFormat: "qcow2"}
	if err := qemuImg.EnsureOverlay(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := qemuImg.EnsureOverlay(context.Background(), spec); err != nil {
		t.Fatalf("validate existing overlay: %v", err)
	}
	info, err := os.Stat(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("overlay mode = %o, want 600", info.Mode().Perm())
	}
}

func TestQEMUImgEnsureOverlayRejectsUnexpectedBacking(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	overlay := filepath.Join(dir, "root.overlay.qcow2")
	if err := os.WriteFile(base, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte("overlay"), 0o600); err != nil {
		t.Fatal(err)
	}
	qemuImg := NewQEMUImg(fakeQEMUImg(t, dir, filepath.Join(dir, "other.qcow2")))
	if err := qemuImg.EnsureOverlay(context.Background(), OverlaySpec{
		Path: overlay, BasePath: base, BaseFormat: "qcow2",
	}); err == nil {
		t.Fatal("expected backing mismatch")
	}
}

func TestQEMUImgRebaseOverlayValidatesLocalBacking(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	overlay := filepath.Join(dir, "root.overlay.qcow2")
	for _, path := range []string{base, overlay} {
		if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	qemuImg := NewQEMUImg(fakeQEMUImg(t, dir, base))
	if err := qemuImg.RebaseOverlay(context.Background(), overlay, base, "qcow2"); err != nil {
		t.Fatal(err)
	}
}

func fakeQEMUImg(t *testing.T, dir, backing string) string {
	t.Helper()
	path := filepath.Join(dir, "qemu-img")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
  create)
    for last do :; done
    : > "$last"
    ;;
  info)
    printf '%%s\n' '{"format":"qcow2","backing-filename":%q,"virtual-size":1048576}'
    ;;
  rebase)
    ;;
  *) exit 2 ;;
esac
`, backing)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
