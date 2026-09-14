package images

import (
	"context"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
)

type ImageResolver interface {
	Resolve(context.Context, string) (Image, error)
}

func Verify(ctx context.Context, paths Paths, catalog ImageResolver, reference string) (result Image, returnErr error) {
	image, err := catalog.Resolve(ctx, reference)
	if err != nil {
		return Image{}, err
	}
	lockPaths := make([]string, len(image.Layers))
	for pos, layer := range image.Layers {
		lockPaths[pos] = paths.Lock(layer.SourceDigest)
	}
	var locks filelock.Set
	if err := locks.Lock(ctx, lockPaths...); err != nil {
		return Image{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Unlock(context.WithoutCancel(ctx))) }()
	image, err = catalog.Resolve(ctx, reference)
	if err != nil {
		return Image{}, err
	}
	var total int64
	for _, layer := range image.Layers {
		if err := verifyLayer(ctx, paths, layer); err != nil {
			return Image{}, errdefs.Context(err, "verify image", reference, "artifacts", "re-import the image", false)
		}
		total += layer.Size
	}
	boot, err := SelectBoot(image.Layers)
	if err != nil || boot != image.Boot || total != image.Size {
		return Image{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("image layer mapping or boot selection is inconsistent"))
	}
	return image, nil
}

func verifyLayer(ctx context.Context, paths Paths, layer Layer) error {
	if layer.SourceDigest.IsZero() || layer.EROFSDigest.IsZero() || layer.Size <= 0 {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("invalid layer metadata"))
	}
	if err := verifyFile(ctx, paths.EROFS(layer.SourceDigest), layer.EROFSDigest, layer.Size); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, file := range layer.BootFiles {
		path, err := paths.BootFile(layer.SourceDigest, file.Name)
		if err != nil {
			return err
		}
		if seen[file.Name] || file.Digest.IsZero() || file.Size <= 0 {
			return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("invalid boot file metadata"))
		}
		seen[file.Name] = true
		if err := verifyFile(ctx, path, file.Digest, file.Size); err != nil {
			return err
		}
	}
	return nil
}

func verifyFile(ctx context.Context, path string, expected Digest, expectedSize int64) error {
	digest, size, err := digestFileContext(ctx, path)
	if err != nil {
		return err
	}
	if digest != expected || size != expectedSize {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("artifact %s does not match metadata", path))
	}
	return nil
}
