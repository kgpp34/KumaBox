package sandbox

import (
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

func TestManagedPaths(t *testing.T) {
	base := t.TempDir()
	paths, err := NewPaths(storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	cow, err := paths.COW(id)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(paths.DataDir(), id.String(), "cow.raw"); cow != want {
		t.Fatalf("COW() = %q, want %q", cow, want)
	}
	if _, err := paths.COW(types.SandboxID("../escape")); err == nil {
		t.Fatal("unsafe ID produced a managed path")
	}
}
