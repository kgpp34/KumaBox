package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/storage"
)

type Options struct {
	Limits      Limits
	Parallelism int
	Now         func() time.Time
}

func DefaultOptions() Options {
	return Options{Limits: DefaultLimits(), Parallelism: min(4, max(1, runtime.NumCPU())), Now: time.Now}
}

type Importer struct {
	paths     Paths
	catalog   Catalog
	converter Converter
	reporter  Reporter
	reportMu  sync.Mutex
	options   Options
}

func NewImporter(paths Paths, catalog Catalog, converter Converter, reporter Reporter, options Options) (*Importer, error) {
	if options.Limits == (Limits{}) {
		options.Limits = DefaultLimits()
	}
	if !options.Limits.Valid() {
		return nil, invalidImage("invalid import size limits")
	}
	if catalog == nil || converter == nil || options.Parallelism <= 0 || options.Now == nil {
		return nil, errors.New("image catalog, converter, parallelism and clock are required")
	}
	if reporter == nil {
		reporter = DiscardReporter{}
	}
	return &Importer{paths: paths, catalog: catalog, converter: converter, reporter: reporter, options: options}, nil
}

func (i *Importer) Import(ctx context.Context, name string, platform Platform, source Source) (result Image, returnErr error) {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n\t") || source == nil || !validPlatform(platform) {
		return Image{}, invalidImage("image name, supported platform and source are required")
	}
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}
	if err := i.paths.Ensure(); err != nil {
		return Image{}, errdefs.Context(err, "import image", name, "prepare", "check managed directory permissions", false)
	}
	manifest, err := source.Resolve(ctx, platform)
	if err != nil {
		return Image{}, errdefs.Context(err, "import image", name, "resolve", "check the OCI source and platform", false)
	}
	if manifest.Digest.IsZero() || manifest.Platform != platform || len(manifest.Layers) == 0 {
		return Image{}, invalidImage("invalid OCI manifest or platform")
	}
	digests := make([]Digest, len(manifest.Layers))
	for position, descriptor := range manifest.Layers {
		if descriptor.Digest.IsZero() || descriptor.Size < 0 || descriptor.Size > i.options.Limits.LayerSize {
			return Image{}, invalidImage("invalid OCI layer descriptor")
		}
		digests[position] = descriptor.Digest
	}
	known, err := i.catalog.FindLayers(ctx, digests)
	if err != nil {
		return Image{}, err
	}
	staging, err := i.paths.NewStaging("image-*")
	if err != nil {
		return Image{}, err
	}
	committed := false
	defer func() {
		if err := removeStaging(staging); err != nil {
			returnErr = errors.Join(returnErr, errdefs.Context(err, "import image", name, "cleanup", "remove orphan staging", committed))
		}
	}()
	converted := make([]ConvertedLayer, len(manifest.Layers))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(i.options.Parallelism)
	for position, descriptor := range manifest.Layers {
		group.Go(func() error {
			if layer, ok := known[descriptor.Digest]; ok && verifyLayer(groupCtx, i.paths, layer) == nil {
				converted[position] = cachedArtifact(i.paths, layer)
			} else {
				workDir := filepath.Join(staging, fmt.Sprintf("layer-%08d", position))
				if err := os.Mkdir(workDir, 0o750); err != nil {
					return err
				}
				artifact, err := i.convert(groupCtx, source, descriptor, workDir)
				if err != nil {
					return err
				}
				converted[position] = artifact
			}
			i.reportMu.Lock()
			defer i.reportMu.Unlock()
			return i.reporter.Layer(position, len(manifest.Layers), descriptor.Digest)
		})
	}
	if err := group.Wait(); err != nil {
		return Image{}, errdefs.Context(err, "import image", name, "convert", "fix source or converter and retry", false)
	}
	lockPaths := make([]string, len(digests))
	for pos, digest := range digests {
		lockPaths[pos] = i.paths.Lock(digest)
	}
	var locks filelock.Set
	if err := locks.Lock(ctx, lockPaths...); err != nil {
		return Image{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, errdefs.Context(locks.Unlock(context.WithoutCancel(ctx)), "import image", name, "unlock", "inspect runtime locks", committed))
	}()
	// Conversion is slow; metadata and files may have changed while we were staging.
	current, err := i.catalog.FindLayers(ctx, digests)
	if err != nil {
		return Image{}, err
	}
	layers := make([]Layer, len(converted))
	for pos, artifact := range converted {
		if err := ctx.Err(); err != nil {
			return Image{}, err
		}
		if layer, ok := current[artifact.SourceDigest]; ok && verifyLayer(ctx, i.paths, layer) == nil {
			layers[pos] = layer
			continue
		}
		// A cache hit may have been removed meanwhile. Retry without downloading under a lock.
		if artifact.EROFSPath == i.paths.EROFS(artifact.SourceDigest) {
			return Image{}, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("cached layer changed during import; retry"))
		}
		layer, err := i.publishLayer(ctx, artifact, staging)
		if err != nil {
			return Image{}, errdefs.Context(err, "import image", name, "publish", "retry the import", false)
		}
		if old, exists := current[layer.SourceDigest]; exists && !sameLayer(old, layer) {
			return Image{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("rebuilt layer differs from committed metadata"))
		}
		current[layer.SourceDigest] = layer
		layers[pos] = layer
	}
	boot, err := selectBoot(layers)
	if err != nil {
		return Image{}, err
	}
	var total int64
	for _, layer := range layers {
		total += layer.Size
	}
	// Revalidate every final artifact while holding all digest locks, before the transaction.
	for _, layer := range layers {
		if err := verifyLayer(ctx, i.paths, layer); err != nil {
			return Image{}, err
		}
	}
	commit := ImportCommit{Name: name, Manifest: manifest, Layers: layers, Boot: boot, Size: total, Created: i.options.Now().UTC()}
	if err := i.catalog.CommitImport(ctx, commit); err != nil {
		return Image{}, errdefs.Context(err, "import image", name, "catalog commit", "retry; unregistered artifacts will be rebuilt", false)
	}
	committed = true
	result, err = i.catalog.Resolve(ctx, name)
	if err != nil {
		return Image{}, errdefs.Context(err, "import image", name, "read committed image", "run image verify", true)
	}
	i.reportMu.Lock()
	defer i.reportMu.Unlock()
	return result, errdefs.Context(i.reporter.Committed(result), "import image", name, "report", "image is committed; run image inspect", true)
}

func (i *Importer) convert(ctx context.Context, source Source, descriptor Descriptor, workDir string) (ConvertedLayer, error) {
	reader, err := source.OpenLayer(ctx, descriptor)
	if err != nil {
		return ConvertedLayer{}, err
	}
	artifact, convertErr := i.converter.Convert(ctx, descriptor, reader, workDir)
	// A converter may stop at the end of tar before the underlying compressed stream ends.
	if convertErr == nil {
		_, convertErr = io.Copy(io.Discard, contextReader{ctx: ctx, reader: reader})
	}
	if err := errors.Join(convertErr, reader.Close()); err != nil {
		return ConvertedLayer{}, fmt.Errorf("convert layer %s: %w", descriptor.Digest, err)
	}
	if artifact.SourceDigest != descriptor.Digest || artifact.EROFSDigest.IsZero() {
		return ConvertedLayer{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, errors.New("converter returned an invalid artifact identity"))
	}
	return artifact, nil
}

func cachedArtifact(paths Paths, layer Layer) ConvertedLayer {
	artifact := ConvertedLayer{SourceDigest: layer.SourceDigest, EROFSPath: paths.EROFS(layer.SourceDigest), EROFSDigest: layer.EROFSDigest, Size: layer.Size, Whiteouts: layer.Whiteouts, BootOpaque: layer.BootOpaque}
	for _, file := range layer.BootFiles {
		artifact.BootFiles = append(artifact.BootFiles, StagedBootFile{Name: file.Name, Path: filepath.Join(paths.BootDir(layer.SourceDigest), file.Name)})
	}
	return artifact
}

func (i *Importer) publishLayer(ctx context.Context, artifact ConvertedLayer, staging string) (Layer, error) {
	layer := Layer{SourceDigest: artifact.SourceDigest, EROFSDigest: artifact.EROFSDigest, Size: artifact.Size, Whiteouts: artifact.Whiteouts, BootOpaque: artifact.BootOpaque}
	// Do not trust a file merely because it already exists: only committed metadata authorizes reuse.
	actual, size, err := stagedDigest(ctx, staging, artifact.EROFSPath)
	if err != nil {
		return Layer{}, err
	}
	if actual != artifact.EROFSDigest || size != artifact.Size || size <= 0 {
		return Layer{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("staged EROFS does not match converter result"))
	}
	for _, file := range artifact.BootFiles {
		if !IsBootName(file.Name) {
			return Layer{}, invalidImage("invalid boot file name")
		}
		digest, size, err := stagedDigest(ctx, staging, file.Path)
		if err != nil {
			return Layer{}, err
		}
		if size == 0 {
			return Layer{}, invalidImage("empty boot file %s", file.Name)
		}
		layer.BootFiles = append(layer.BootFiles, BootFile{Name: file.Name, Digest: digest, Size: size})
	}
	// Check against every existing committed mapping BEFORE replacing shared files.
	known, err := i.catalog.FindLayers(ctx, []Digest{layer.SourceDigest})
	if err != nil {
		return Layer{}, err
	}
	if old, exists := known[layer.SourceDigest]; exists && !sameLayer(old, layer) {
		return Layer{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("rebuilt layer differs from committed metadata"))
	}
	if err := storage.Publish(artifact.EROFSPath, i.paths.EROFS(artifact.SourceDigest)); err != nil {
		return Layer{}, err
	}
	for _, file := range artifact.BootFiles {
		final, err := i.paths.BootFile(artifact.SourceDigest, file.Name)
		if err != nil {
			return Layer{}, err
		}
		if err := storage.Publish(file.Path, final); err != nil {
			return Layer{}, err
		}
	}
	return layer, nil
}

func stagedDigest(ctx context.Context, staging, path string) (Digest, int64, error) {
	rel, err := filepath.Rel(staging, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Digest{}, 0, invalidImage("converter artifact escapes staging")
	}
	return digestFileContext(ctx, path)
}

func selectBoot(layers []Layer) (Boot, error) {
	type candidate struct {
		layer Digest
		file  BootFile
	}
	var candidates []candidate
	for _, layer := range layers {
		if layer.BootOpaque {
			candidates = nil
		}
		for _, name := range layer.Whiteouts {
			var kept []candidate
			for _, c := range candidates {
				if c.file.Name != name {
					kept = append(kept, c)
				}
			}
			candidates = kept
		}
		for _, file := range layer.BootFiles {
			var kept []candidate
			for _, c := range candidates {
				if c.file.Name != file.Name {
					kept = append(kept, c)
				}
			}
			kept = append(kept, candidate{layer: layer.SourceDigest, file: file})
			candidates = kept
		}
	}
	var boot Boot
	for _, c := range candidates {
		if strings.HasPrefix(c.file.Name, "vmlinuz") {
			boot.KernelLayer, boot.KernelFile = c.layer, c.file.Name
		}
		if strings.HasPrefix(c.file.Name, "initrd.img") {
			boot.InitrdLayer, boot.InitrdFile = c.layer, c.file.Name
		}
	}
	if boot.KernelLayer.IsZero() {
		return Boot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("image is missing a regular /boot/vmlinuz* kernel"))
	}
	if boot.InitrdLayer.IsZero() {
		return Boot{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("image is missing a regular /boot/initrd.img* initrd"))
	}
	return boot, nil
}

func validPlatform(p Platform) bool {
	return p.OS == "linux" && (p.Architecture == "amd64" || p.Architecture == "arm64")
}

func invalidImage(format string, args ...any) error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf(format, args...))
}
func validFile(path string) bool { return storage.CheckPath(path) == nil && regularNonempty(path) }
func regularNonempty(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func removeStaging(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove staging %s: %w", path, err)
	}
	return nil
}
