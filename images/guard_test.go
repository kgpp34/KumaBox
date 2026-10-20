package images

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

type changingResolver struct {
	first  types.Image
	second types.Image
	calls  int
}

func (r *changingResolver) Resolve(context.Context, string) (types.Image, error) {
	r.calls++
	if r.calls == 1 {
		return r.first, nil
	}
	return r.second, nil
}

func TestGuardRejectsImageBindingChangedWhileWaitingForLocks(t *testing.T) {
	first, err := types.ParseDigest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	second, err := types.ParseDigest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
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
	resolver := &changingResolver{first: types.Image{ManifestDigest: first}, second: types.Image{ManifestDigest: second}}
	used := false
	_, err = NewGuard(paths, resolver).WithAvailable(t.Context(), "demo", func(types.Image) error {
		used = true
		return nil
	})
	if code, _ := errdefs.CodeOf(err); code != errdefs.CodeStateConflict {
		t.Fatalf("WithAvailable error = %v", err)
	}
	if used {
		t.Fatal("consumer ran after image binding changed")
	}
}
