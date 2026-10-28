package images

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

type changingResolver struct {
	first  types.Image
	second types.Image
	calls  int
}

// TestGuardReadersOverlapAndKeepRemovalBlocked constrains the lifecycle
// contract: readers can reserve independently, but no layer may disappear while
// either reader is committing its durable sandbox reference.
func TestGuardReadersOverlapAndKeepRemovalBlocked(t *testing.T) {
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
	digest, err := types.ParseDigest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	image := types.Image{ManifestDigest: digest, Size: 1, Layers: []types.Layer{{
		SourceDigest: digest, EROFSDigest: digest, Size: 1,
		BootFiles: []types.BootFile{{Name: "vmlinuz", Digest: digest, Size: 1}, {Name: "initrd.img", Digest: digest, Size: 1}},
	}}}
	image.Boot, err = SelectBoot(image.Layers)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.EROFS(digest), paths.Kernel(digest), paths.Initrd(digest)} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte{1}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	guard := NewGuard(paths, fixedImageResolver{image})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	entered, finished := make(chan struct{}, 2), make(chan error, 2)
	release := make(chan struct{})
	defer close(release)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := guard.WithAvailable(ctx, "demo", func(types.Image) error {
				entered <- struct{}{}
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			finished <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case err := <-finished:
			t.Fatalf("reader finished before both could enter: %v", err)
		case <-ctx.Done():
			t.Fatal("image readers serialized instead of sharing the layer lease")
		}
	}
	writer := filelock.New(paths.Lock(digest))
	if ok, err := writer.TryLock(ctx); err != nil || ok {
		_ = writer.Unlock(ctx)
		t.Fatalf("exclusive artifact writer passed active readers: %v, %v", ok, err)
	}
	// Releasing via context also proves that failed consumers give up the lease.
	cancel()
	for range 2 {
		if err := <-finished; !errors.Is(err, context.Canceled) {
			t.Fatalf("consumer cancellation: %v", err)
		}
	}
	if ok, err := writer.TryLock(t.Context()); err != nil || !ok {
		t.Fatalf("artifact writer remained blocked after consumers exited: %v, %v", ok, err)
	}
	if err := writer.Unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type fixedImageResolver struct{ image types.Image }

func (r fixedImageResolver) Resolve(context.Context, string) (types.Image, error) {
	return r.image, nil
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
