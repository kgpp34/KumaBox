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

// Source resolves an image and supplies verified, decompressed layer tar streams.
// Implementations retain ownership of archive staging or registry state; callers
// must keep the source alive until Import returns.
type Source interface {
	// Resolve selects exactly one manifest matching the requested platform.
	Resolve(context.Context, Platform) (Manifest, error)
	// OpenLayer returns a stream whose final read or Close may report verification
	// failures. After successful conversion the importer drains the remaining bytes;
	// every successfully opened stream is closed, including on conversion failure.
	OpenLayer(context.Context, Descriptor) (io.ReadCloser, error)
}

// ImportCatalog provides the metadata operations needed by an import.
type ImportCatalog interface {
	// Resolve reads an image by its local alias or manifest identity.
	Resolve(context.Context, string) (Image, error)
	// FindLayers returns committed artifact mappings for the requested source blobs.
	FindLayers(context.Context, []Digest) (map[Digest]Layer, error)
	// CommitImport atomically registers the image, alias and ordered layer mappings.
	CommitImport(context.Context, ImportCommit) error
}

// Converter builds layer artifacts in an importer-owned staging directory.
// Convert may run concurrently for independent layers and must honor cancellation.
type Converter interface {
	// Convert consumes a decompressed tar stream and returns staged artifacts.
	// Artifact paths must remain within the supplied work directory.
	Convert(context.Context, Descriptor, io.Reader, string) (ConvertedLayer, error)
}

// ConvertedLayer contains files awaiting validation and publication by Importer.
type ConvertedLayer struct {
	// SourceDigest must match the descriptor that was converted.
	SourceDigest Digest
	// EROFSPath is the staged EROFS file, or a verified managed file on cache reuse.
	EROFSPath string
	// EROFSDigest is the expected hash of EROFSPath.
	EROFSDigest Digest
	// Size is the expected EROFS size in bytes.
	Size int64
	// BootFiles contains staged regular boot candidates.
	BootFiles []StagedBootFile
	// Whiteouts names lower-layer boot candidates hidden by this layer.
	Whiteouts []string
	// BootOpaque hides the entire inherited boot candidate set.
	BootOpaque bool
}

// StagedBootFile locates a boot candidate before publication and metadata hashing.
type StagedBootFile struct {
	// Name is the final accepted /boot basename.
	Name string
	// Path is a file under import staging, or the managed path on cache reuse.
	Path string
}

// Reporter observes completed import work. The importer serializes callbacks;
// layer callbacks follow completion order, not necessarily manifest order.
// Reporting errors abort work or report a failure after metadata was committed.
type Reporter interface {
	// Layer receives the zero-based manifest position, total count and source digest.
	Layer(int, int, Digest) error
	// Committed runs after the atomic catalog commit and readback succeed.
	Committed(Image) error
}

// DiscardReporter disables progress reporting without conditional workflow logic.
type DiscardReporter struct{}

// Layer accepts a layer completion without retaining it.
func (DiscardReporter) Layer(int, int, Digest) error { return nil }

// Committed accepts a successful catalog commit without retaining it.
func (DiscardReporter) Committed(Image) error { return nil }

// Limits bound compressed input and decompressed source and boot artifacts.
// Adapters enforce the relevant bounds while reading; all values are byte counts.
type Limits struct {
	// LayerSize caps each original layer blob.
	LayerSize int64
	// UnpackedSize caps each decompressed layer tar stream.
	UnpackedSize int64
	// BootSize caps each extracted boot file after optional decompression.
	BootSize int64
	// ArchiveSize caps the total expanded file content of a local archive.
	ArchiveSize int64
}

// DefaultLimits returns bounds for source blobs, unpacked tar and boot artifacts.
func DefaultLimits() Limits {
	return Limits{LayerSize: 8 << 30, UnpackedSize: 16 << 30, BootSize: 512 << 20, ArchiveSize: 32 << 30}
}

// Valid requires positive bounds with room for a one-byte overflow probe.
func (l Limits) Valid() bool {
	return l.LayerSize > 0 && l.UnpackedSize > 0 && l.BootSize > 0 && l.ArchiveSize > 0 && l.LayerSize < 1<<63-1 && l.UnpackedSize < 1<<63-1 && l.BootSize < 1<<63-1 && l.ArchiveSize < 1<<63-1
}

// Options controls resource bounds, conversion concurrency and import timestamps.
type Options struct {
	// Limits supplies adapter bounds; an all-zero value selects DefaultLimits.
	Limits Limits
	// Parallelism caps concurrent layer reuse checks and conversions; it must be positive.
	Parallelism int
	// Now supplies the local creation timestamp and must be non-nil.
	Now func() time.Time
}

// DefaultOptions limits conversions to at most four workers and uses the wall clock.
func DefaultOptions() Options {
	return Options{Limits: DefaultLimits(), Parallelism: min(4, max(1, runtime.NumCPU())), Now: time.Now}
}

// Importer coordinates format-independent conversion, shared artifacts and metadata.
// Sources and converters enforce stream limits; the importer validates identities
// and artifacts before committing their catalog mappings.
type Importer struct {
	// paths defines shared artifacts, per-import staging and source digest locks.
	paths Paths
	// catalog owns the atomic registration and alias reachability boundary.
	catalog ImportCatalog
	// converter builds artifacts outside locks so slow source reads cannot block deletion.
	converter Converter
	// reporter receives serialized layer completions and the committed result.
	reporter Reporter
	// reportMu serializes callbacks from concurrent conversion workers.
	reportMu sync.Mutex
	// options fixes this importer's limits, worker count and creation clock.
	options Options
}

// NewImporter validates workflow dependencies and options. A nil reporter discards
// progress; zero Limits selects defaults, while parallelism and clock are required.
func NewImporter(paths Paths, catalog ImportCatalog, converter Converter, reporter Reporter, options Options) (*Importer, error) {
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

// Import registers name for a resolved image, reusing only verified committed layers.
// Source resolution and conversion happen outside artifact locks. Publication and
// the catalog transaction hold every source digest lock to coordinate with removal.
// Errors after commit carry errdefs.Error.Committed so callers can distinguish a
// persisted image from an import that must be retried.
//
//	resolve -> reuse/convert in staging -> lock all source digests
//	                                      |
//	                                      v
//	                    recheck -> publish -> verify -> catalog commit
//	                                                       |
//	                                                       v
//	                                              report -> unlock -> staging cleanup
func (i *Importer) Import(ctx context.Context, name string, platform Platform, source Source) (result Image, returnErr error) {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n\t") || source == nil || !platform.Valid() {
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
		return Image{}, errdefs.Context(err, "import image", name, "resolve", "check the image source and platform", false)
	}
	if manifest.Digest.IsZero() || manifest.Platform != platform || len(manifest.Layers) == 0 {
		return Image{}, invalidImage("invalid image manifest or platform")
	}
	digests := make([]Digest, len(manifest.Layers))
	for position, descriptor := range manifest.Layers {
		if descriptor.Digest.IsZero() || descriptor.Size < 0 || descriptor.Size > i.options.Limits.LayerSize {
			return Image{}, invalidImage("invalid layer descriptor")
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
		if old, exists := current[layer.SourceDigest]; exists && !old.Equal(layer) {
			return Image{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("rebuilt layer differs from committed metadata"))
		}
		current[layer.SourceDigest] = layer
		layers[pos] = layer
	}
	boot, err := SelectBoot(layers)
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

// convert drains the source after tar processing so trailing hash, compression
// and size checks cannot be bypassed by a converter that stops at tar EOF.
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

// cachedArtifact adapts verified committed files to the staging result shape.
// Their managed paths let publication detect a cache entry removed during staging.
func cachedArtifact(paths Paths, layer Layer) ConvertedLayer {
	artifact := ConvertedLayer{SourceDigest: layer.SourceDigest, EROFSPath: paths.EROFS(layer.SourceDigest), EROFSDigest: layer.EROFSDigest, Size: layer.Size, Whiteouts: layer.Whiteouts, BootOpaque: layer.BootOpaque}
	for _, file := range layer.BootFiles {
		artifact.BootFiles = append(artifact.BootFiles, StagedBootFile{Name: file.Name, Path: filepath.Join(paths.BootDir(layer.SourceDigest), file.Name)})
	}
	return artifact
}

// publishLayer validates staged hashes and committed mappings before replacing
// shared files. The caller must hold the source digest lock throughout publication.
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
	if old, exists := known[layer.SourceDigest]; exists && !old.Equal(layer) {
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

// stagedDigest rejects converter paths outside this import before hashing files.
func stagedDigest(ctx context.Context, staging, path string) (Digest, int64, error) {
	rel, err := filepath.Rel(staging, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Digest{}, 0, invalidImage("converter artifact escapes staging")
	}
	return digestFileContext(ctx, path)
}

func invalidImage(format string, args ...any) error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func removeStaging(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove staging %s: %w", path, err)
	}
	return nil
}
