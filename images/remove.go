package images

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"

	filelock "github.com/kumabox/kumabox/lock/flock"
)

// RemovalCatalog exposes only the metadata operations needed for safe deletion.
type RemovalCatalog interface {
	ImageResolver
	// Remove atomically deletes references only if the manifest binding is unchanged.
	Remove(context.Context, string, Digest) (Removal, error)
}

// Remove drops an alias or a manifest and deletes only layers no longer referenced
// by the catalog. It holds source digest locks across metadata removal and file
// cleanup so an importer cannot reuse files while they are being deleted.
// Cleanup failures are reported as committed: removed metadata is not restored.
//
//	resolve -> lock layers -> remove metadata -> delete unreferenced files -> unlock
//	                           (atomic)           (best effort)
func Remove(ctx context.Context, paths Paths, catalog RemovalCatalog, reference string) (result Removal, returnErr error) {
	image, err := catalog.Resolve(ctx, reference)
	if err != nil {
		return Removal{}, err
	}
	lockPaths := make([]string, len(image.Layers))
	for position, layer := range image.Layers {
		lockPaths[position] = paths.Lock(layer.SourceDigest)
	}
	var locks filelock.Set
	if err := locks.Lock(ctx, lockPaths...); err != nil {
		return Removal{}, fmt.Errorf("lock image layers: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Unlock(context.WithoutCancel(ctx))) }()
	result, err = catalog.Remove(ctx, reference, image.ManifestDigest)
	if err != nil {
		return Removal{}, err
	}
	var cleanup []error
	for _, digest := range result.Layers {
		for _, path := range []string{paths.EROFS(digest), paths.BootDir(digest)} {
			if err := storage.CheckPath(path); err != nil {
				cleanup = append(cleanup, err)
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				cleanup = append(cleanup, fmt.Errorf("remove image artifact %s: %w", path, err))
			}
		}
	}
	return result, errdefs.Context(errors.Join(cleanup...), "remove image", reference, "cleanup", "metadata removed; orphan artifacts can be reclaimed", true)
}
