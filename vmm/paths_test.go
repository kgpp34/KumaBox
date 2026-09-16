package vmm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

const testSandboxID = types.SandboxID("123e4567-e89b-42d3-a456-426614174000")

func TestPathsRoundTripPrivateProcessIdentity(t *testing.T) {
	base := t.TempDir()
	paths, err := NewPaths(storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")})
	if err != nil {
		t.Fatal(err)
	}
	id := testSandboxID
	if err := paths.Prepare(id); err != nil {
		t.Fatal(err)
	}
	runDir, _ := paths.RunDir(id)
	if info, err := os.Stat(runDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime directory = %+v, %v", info, err)
	}
	process := Process{PID: 42, StartTicks: 7, BootID: "boot", SandboxID: id, Generation: 3, Binary: "cloud-hypervisor", APISocket: filepath.Join(runDir, "api.sock")}
	if err := paths.WriteProcess(process); err != nil {
		t.Fatal(err)
	}
	got, err := paths.ReadProcess(id)
	if err != nil {
		t.Fatal(err)
	}
	if got != process {
		t.Fatalf("process = %+v, want %+v", got, process)
	}
	if err := paths.Clear(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("runtime directory remains: %v", err)
	}
}
