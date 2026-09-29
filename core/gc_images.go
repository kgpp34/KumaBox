package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kumabox/kumabox/images"
	imagescatalog "github.com/kumabox/kumabox/images/catalog"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

// discoverImageArtifacts reads only source-digest-shaped artifacts. An import
// holds that digest's lock from publication through catalog commit, so a later
// collection pass can safely distinguish unreferenced files from active writes.
func discoverImageArtifacts(paths images.Paths) (map[types.Digest]bool, error) {
	candidates := make(map[types.Digest]bool)
	for _, directory := range []string{paths.LayersDir(), paths.BootBaseDir()} {
		if err := storage.CheckPath(directory); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(directory)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if directory == paths.LayersDir() {
				if !strings.HasSuffix(name, ".erofs") {
					continue
				}
				name = strings.TrimSuffix(name, ".erofs")
			}
			digest, err := types.ParseDigest("sha256:" + name)
			if err != nil {
				continue
			}
			if directory == paths.LayersDir() && !entry.Type().IsRegular() || directory == paths.BootBaseDir() && !entry.IsDir() {
				return nil, fmt.Errorf("managed image artifact %s has an invalid file type", filepath.Join(directory, entry.Name()))
			}
			candidates[digest] = true
		}
	}
	return candidates, nil
}

func sortedImageDigests(candidates map[types.Digest]bool) []types.Digest {
	ordered := make([]types.Digest, 0, len(candidates))
	for digest := range candidates {
		ordered = append(ordered, digest)
	}
	slices.SortFunc(ordered, func(left, right types.Digest) int {
		return strings.Compare(left.String(), right.String())
	})
	return ordered
}

// collectOrphanImage rechecks layer references inside the source digest lock.
// An importer or remover holding the same lock is retried on the next pass.
func (s *SnapshotService) collectOrphanImage(ctx context.Context, paths images.Paths, digest types.Digest) (collected, busy bool, returnErr error) {
	lock := filelock.New(paths.Lock(digest))
	acquired, err := lock.TryLock(ctx)
	if err != nil || !acquired {
		return false, !acquired, err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx))) }()
	referenced, err := imagescatalog.New(s.store).FindLayers(ctx, []types.Digest{digest})
	if err != nil {
		return false, false, err
	}
	if _, exists := referenced[digest]; exists {
		return false, false, nil
	}
	for _, artifact := range []string{paths.EROFS(digest), paths.BootDir(digest)} {
		if err := storage.CheckPath(artifact); err != nil {
			return false, false, err
		}
		if err := os.RemoveAll(artifact); err != nil {
			return false, false, fmt.Errorf("remove image artifact %s: %w", artifact, err)
		}
	}
	return true, false, nil
}
