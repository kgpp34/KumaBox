package core

import (
	"os"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/images"
	imagescatalog "github.com/kumabox/kumabox/images/catalog"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/types"
)

func TestCollectOrphanImageRemovesUnreferencedArtifacts(t *testing.T) {
	paths, err := images.NewPaths(gcTestRoots(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.EROFS(digest), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.BootDir(digest), 0o700); err != nil {
		t.Fatal(err)
	}
	candidates, err := discoverImageArtifacts(paths)
	if err != nil || !candidates[digest] {
		t.Fatalf("image discovery = %v, %v", candidates, err)
	}
	store, err := metadata.NewMemory(imagescatalog.Collections())
	if err != nil {
		t.Fatal(err)
	}
	service := &SnapshotService{store: store}
	collected, busy, err := service.collectOrphanImage(t.Context(), paths, digest)
	if err != nil || busy || !collected {
		t.Fatalf("image collection = %t, %t, %v", collected, busy, err)
	}
	for _, artifact := range []string{paths.EROFS(digest), paths.BootDir(digest)} {
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Fatalf("orphan image artifact %s remains: %v", artifact, err)
		}
	}
}

func TestCollectOrphanImageSkipsBusyDigest(t *testing.T) {
	paths, err := images.NewPaths(gcTestRoots(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	digest, err := types.ParseDigest("sha256:" + strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	artifact := paths.EROFS(digest)
	if err := os.WriteFile(artifact, []byte("active publication"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := filelock.New(paths.Lock(digest))
	if err := owner.Lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Unlock(t.Context()) }()
	service := &SnapshotService{}
	collected, busy, err := service.collectOrphanImage(t.Context(), paths, digest)
	if err != nil || !busy || collected {
		t.Fatalf("image collection = %t, %t, %v", collected, busy, err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("busy image artifact removed: %v", err)
	}
}
