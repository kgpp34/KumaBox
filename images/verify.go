package images

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/types"
)

// ImageResolver is the read-only catalog contract required for verification.
type ImageResolver interface {
	// Resolve reads aliases or manifest references from committed metadata.
	Resolve(context.Context, string) (types.Image, error)
}

// Guard holds image artifact locks while a consumer checks and commits a reference.
type Guard struct {
	// paths locates artifacts and the locks shared with import and removal.
	paths Paths
	// catalog resolves image facts before and after lock acquisition.
	catalog ImageResolver
}

// NewGuard constructs an image guard for lifecycle consumers.
func NewGuard(paths Paths, catalog ImageResolver) *Guard {
	return &Guard{paths: paths, catalog: catalog}
}

// WithAvailable checks regular-file presence and size while holding layer locks
// across use. Full content hashing remains the explicit Verify operation so create
// latency does not grow with total image bytes.
func (g *Guard) WithAvailable(ctx context.Context, reference string, use func(types.Image) error) (types.Image, error) {
	return g.withLocked(ctx, reference, availableImage, use)
}

// withLocked closes the remove/use race by resolving again after lock acquisition
// and retaining those locks until the consumer commits its reference.
func (g *Guard) withLocked(ctx context.Context, reference string, check func(context.Context, Paths, types.Image) error, use func(types.Image) error) (result types.Image, returnErr error) {
	if g == nil || g.catalog == nil || use == nil {
		return types.Image{}, errors.New("image guard is not configured")
	}
	image, err := g.catalog.Resolve(ctx, reference)
	if err != nil {
		return types.Image{}, err
	}
	expected := image.ManifestDigest
	lockPaths := make([]string, len(image.Layers))
	for pos, layer := range image.Layers {
		lockPaths[pos] = g.paths.Lock(layer.SourceDigest)
	}
	var locks filelock.Set
	if err := locks.Lock(ctx, lockPaths...); err != nil {
		return types.Image{}, fmt.Errorf("lock image layers: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, locks.Unlock(context.WithoutCancel(ctx))) }()
	image, err = g.catalog.Resolve(ctx, reference)
	if err != nil {
		return types.Image{}, err
	}
	if image.ManifestDigest != expected {
		return types.Image{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("image binding changed while waiting for layer locks; retry"))
	}
	if err := check(ctx, g.paths, image); err != nil {
		return types.Image{}, err
	}
	if err := use(image); err != nil {
		return types.Image{}, err
	}
	return image, nil
}

// Verify hashes every EROFS and extracted boot artifact and checks derived image
// facts. It holds source digest locks to coordinate with publication and deletion.
// The reference is resolved again after waiting for locks to detect removal.
func Verify(ctx context.Context, paths Paths, catalog ImageResolver, reference string) (result types.Image, returnErr error) {
	image, err := NewGuard(paths, catalog).withLocked(ctx, reference, verifyImage, func(types.Image) error { return nil })
	return image, errdefs.Context(err, "verify image", reference, "artifacts", "re-import the image", false)
}

// availableImage checks bounded metadata and filesystem facts without reading full artifacts.
func availableImage(ctx context.Context, paths Paths, image types.Image) error {
	for _, layer := range image.Layers {
		if err := availableFile(ctx, paths.EROFS(layer.SourceDigest), layer.Size); err != nil {
			return err
		}
		for _, file := range layer.BootFiles {
			path, err := paths.BootFile(layer.SourceDigest, file.Name)
			if err != nil {
				return err
			}
			if err := availableFile(ctx, path, file.Size); err != nil {
				return err
			}
		}
	}
	return validateFacts(image)
}

// availableFile rejects missing, replaced, or truncated managed artifacts.
func availableFile(ctx context.Context, path string, expectedSize int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	}
	if !info.Mode().IsRegular() || info.Size() != expectedSize {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("artifact %s is not a regular %d-byte file", path, expectedSize))
	}
	return nil
}

// verifyImage performs the explicit byte-for-byte integrity operation.
func verifyImage(ctx context.Context, paths Paths, image types.Image) error {
	for _, layer := range image.Layers {
		if err := verifyLayer(ctx, paths, layer); err != nil {
			return err
		}
	}
	return validateFacts(image)
}

// validateFacts proves that ordered layers still derive the committed boot and size.
func validateFacts(image types.Image) error {
	var total int64
	for _, layer := range image.Layers {
		total += layer.Size
	}
	boot, err := SelectBoot(image.Layers)
	if err != nil || boot != image.Boot || total != image.Size {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("image layer mapping or boot selection is inconsistent"))
	}
	return nil
}

// verifyLayer proves that a committed mapping still matches all managed files.
// Imports use the same check before authorizing cache reuse.
func verifyLayer(ctx context.Context, paths Paths, layer types.Layer) error {
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

// verifyFile requires both identity and byte size to match committed metadata.
func verifyFile(ctx context.Context, path string, expected types.Digest, expectedSize int64) error {
	digest, size, err := digestFileContext(ctx, path)
	if err != nil {
		return err
	}
	if digest != expected || size != expectedSize {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("artifact %s does not match metadata", path))
	}
	return nil
}
