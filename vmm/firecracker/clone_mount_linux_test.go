//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyCloneDrivesRequiresTargetInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target.raw")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck
	if err := verifyCloneDrives(os.Getpid(), []cloneDiskBinding{{id: "disk0", target: path}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".opened"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyCloneDrives(os.Getpid(), []cloneDiskBinding{{id: "disk0", target: path}}); err == nil {
		t.Fatal("accepted a replaced target that is not open")
	}
}
