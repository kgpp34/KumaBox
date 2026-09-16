// Package catalog adapts transactional metadata storage to image management.
// It stores manifest facts, local aliases and ordered layer references together;
// filesystem publication and reclamation remain the images workflows' responsibility.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/types"
)

var _ images.Catalog = (*Store)(nil)

const (
	// CollectionImages stores one fact record per manifest digest.
	CollectionImages metadata.Collection = "images"
	// CollectionNames maps each local alias to a manifest digest.
	CollectionNames metadata.Collection = "image_names"
	// CollectionLayers stores layer occurrences keyed by manifest and position.
	CollectionLayers metadata.Collection = "image_layers"
	// minimumDigestPrefix limits accidental matches from short hexadecimal names.
	minimumDigestPrefix = 12
)

// Collections returns the collections that must be registered with the metadata engine.
func Collections() []metadata.Collection {
	return []metadata.Collection{CollectionImages, CollectionNames, CollectionLayers}
}

// Store implements the image catalog over an already-open metadata store.
// It does not own or close that store, and it never reads or modifies artifact files.
type Store struct {
	// store supplies snapshot reads and atomic writes for all image collections.
	store metadata.Store
	// usage checks cross-module references before the final manifest is removed.
	usage ImageUsage
}

// ImageUsage checks sandbox references from inside the image removal transaction.
// Implementations must use reader directly and must not open a nested transaction.
type ImageUsage interface {
	InUse(context.Context, metadata.Reader, types.Digest) (bool, error)
}

// Option configures optional cross-module catalog policies.
type Option func(*Store)

// WithImageUsage prevents removal of a final manifest while another module uses it.
func WithImageUsage(usage ImageUsage) Option {
	return func(store *Store) { store.usage = usage }
}

// New creates an adapter for a non-nil metadata store whose schema includes
// Collections. The caller retains ownership of the store's lifetime. A catalog
// sharing metadata with sandboxes must supply WithImageUsage.
func New(store metadata.Store, options ...Option) *Store {
	result := &Store{store: store}
	for _, option := range options {
		option(result)
	}
	return result
}

// Reader resolves image facts from a transaction owned by another module.
// It is stateless so core can connect catalogs without creating an import cycle.
type Reader struct{}

// Resolve reads an alias or digest from the supplied transaction snapshot.
func (Reader) Resolve(ctx context.Context, reader metadata.Reader, reference string) (types.Image, error) {
	return resolveRecord(ctx, reader, reference)
}

// imageRecord stores manifest-wide facts separately from aliases and layer order.
type imageRecord struct {
	// ManifestDigest must match this record's collection key.
	ManifestDigest string `json:"manifest_digest"`
	// OS and Architecture select the supported source platform.
	OS string `json:"os"`
	// Architecture is the platform instruction set, independent of the host.
	Architecture string `json:"architecture"`
	// BootProfile is the versioned early-userspace contract declared by the image.
	// Missing values preserve compatibility with records written before profiles.
	BootProfile string `json:"boot_profile,omitempty"`
	// KernelLayer identifies the source layer owning the selected kernel.
	KernelLayer string `json:"kernel_layer"`
	// KernelFile is the selected regular boot basename.
	KernelFile string `json:"kernel_file"`
	// InitrdFile is the selected regular initrd basename.
	InitrdFile string `json:"initrd_file"`
	// InitrdLayer identifies the source layer owning the selected initrd.
	InitrdLayer string `json:"initrd_layer"`
	// Size sums converted layer occurrences in bytes.
	Size int64 `json:"size"`
	// CreatedAt preserves the first local registration time when aliases are added.
	CreatedAt time.Time `json:"created_at"`
}

// nameRecord allows multiple local aliases to refer to one manifest record.
type nameRecord struct {
	// ManifestDigest is the canonical key in CollectionImages.
	ManifestDigest string `json:"manifest_digest"`
}

// layerRecord represents a manifest occurrence, not a globally unique layer row.
// Repeated source digests retain separate positions but must agree on artifact facts.
type layerRecord struct {
	// ManifestDigest identifies the image containing this occurrence.
	ManifestDigest string `json:"manifest_digest"`
	// Position is zero-based; stored positions must be contiguous.
	Position int `json:"position"`
	// SourceDigest keys shared converted artifacts on disk.
	SourceDigest string `json:"source_digest"`
	// EROFSDigest verifies the converted filesystem bytes.
	EROFSDigest string `json:"erofs_digest"`
	// Size is the converted filesystem size in bytes.
	Size int64 `json:"size"`
	// BootFiles stores extracted regular candidates before cross-layer selection.
	BootFiles []bootFileRecord `json:"boot_files"`
	// Whiteouts names candidates hidden in lower layers.
	Whiteouts []string `json:"whiteouts"`
	// BootOpaque discards all inherited boot candidates.
	BootOpaque bool `json:"boot_opaque"`
}

// bootFileRecord verifies an extracted boot file independently of its source tar.
type bootFileRecord struct {
	// Name is an accepted boot basename under the source layer's boot directory.
	Name string `json:"name"`
	// Digest hashes the extracted file after any kernel decompression.
	Digest string `json:"digest"`
	// Size is the extracted file size in bytes.
	Size int64 `json:"size"`
}

func encodeBootFiles(files []types.BootFile) []bootFileRecord {
	result := make([]bootFileRecord, 0, len(files))
	for _, file := range files {
		result = append(result, bootFileRecord{Name: file.Name, Digest: file.Digest.String(), Size: file.Size})
	}
	return result
}

// Resolve prefers an exact local alias, then accepts a unique manifest digest
// prefix of at least 12 hex characters, with or without the sha256: prefix.
func (c *Store) Resolve(ctx context.Context, reference string) (types.Image, error) {
	var result types.Image
	err := c.store.View(ctx, func(reader metadata.Reader) error {
		image, err := (Reader{}).Resolve(ctx, reader, reference)
		if err != nil {
			return err
		}
		result = image
		return nil
	})
	return result, errdefs.Context(err, "resolve image", reference, "metadata", "check the image name or digest", false)
}

// List loads a consistent snapshot and sorts images by full manifest digest.
// Each image includes sorted aliases and manifest-ordered layer occurrences.
func (c *Store) List(ctx context.Context) ([]types.Image, error) {
	result := make([]types.Image, 0)
	err := c.store.View(ctx, func(reader metadata.Reader) error {
		return reader.Scan(ctx, CollectionImages, func(id string, _ []byte) error {
			image, err := loadImage(ctx, reader, id)
			if err != nil {
				return err
			}
			result = append(result, image)
			return nil
		})
	})
	slices.SortFunc(result, func(left, right types.Image) int {
		return strings.Compare(left.ManifestDigest.String(), right.ManifestDigest.String())
	})
	return result, errdefs.Context(err, "list images", "", "metadata", "inspect the metadata store", false)
}

// FindLayers returns committed mappings for requested source digests.
// Conflicting mappings across images are corruption rather than reusable cache entries.
func (c *Store) FindLayers(ctx context.Context, digests []types.Digest) (map[types.Digest]types.Layer, error) {
	wanted := make(map[types.Digest]struct{}, len(digests))
	for _, digest := range digests {
		wanted[digest] = struct{}{}
	}
	result := make(map[types.Digest]types.Layer)
	err := c.store.View(ctx, func(reader metadata.Reader) error {
		return reader.Scan(ctx, CollectionLayers, func(_ string, raw []byte) error {
			var record layerRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return corruptRecord("layer", err)
			}
			layer, err := decodeLayer(record)
			if err != nil {
				return err
			}
			if _, ok := wanted[layer.SourceDigest]; ok {
				if previous, ok := result[layer.SourceDigest]; ok && !previous.Equal(layer) {
					return corruptRecord("shared layer", errors.New("conflicting artifact metadata"))
				}
				result[layer.SourceDigest] = layer
			}
			return nil
		})
	})
	return result, err
}

// CommitImport atomically binds an alias and stores validated image facts.
// Reimporting the same manifest adds an alias without changing its creation time;
// altered manifest facts or an alias bound to a different digest are rejected.
func (c *Store) CommitImport(ctx context.Context, commit images.ImportCommit) error {
	if err := commit.Validate(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	err := c.store.Update(ctx, func(writer metadata.Writer) error {
		rawName, exists, err := writer.Get(ctx, CollectionNames, commit.Name)
		if err != nil {
			return err
		}
		if exists {
			var current nameRecord
			if err := json.Unmarshal(rawName, &current); err != nil {
				return corruptRecord("name", err)
			}
			if current.ManifestDigest != commit.Manifest.Digest.String() {
				return errdefs.New(errdefs.ClassConflict, errdefs.CodeNameTaken, fmt.Errorf("image name %q already points to %s", commit.Name, current.ManifestDigest))
			}
		}
		if _, exists, err := writer.Get(ctx, CollectionImages, commit.Manifest.Digest.String()); err != nil {
			return err
		} else if exists {
			existing, err := loadImage(ctx, writer, commit.Manifest.Digest.String())
			if err != nil {
				return err
			}
			if existing.Platform != commit.Manifest.Platform || existing.Boot != commit.Boot || existing.Size != commit.Size || len(existing.Layers) != len(commit.Layers) {
				return corruptRecord("image", errors.New("manifest facts changed"))
			}
			for pos, layer := range existing.Layers {
				if !layer.Equal(commit.Layers[pos]) {
					return corruptRecord("image", errors.New("manifest layers changed"))
				}
			}
			return putJSON(ctx, writer, CollectionNames, commit.Name, nameRecord{ManifestDigest: commit.Manifest.Digest.String()})
		}
		record := imageRecord{
			ManifestDigest: commit.Manifest.Digest.String(), OS: commit.Manifest.Platform.OS,
			Architecture: commit.Manifest.Platform.Architecture, BootProfile: string(commit.Boot.Profile),
			KernelLayer: commit.Boot.KernelLayer.String(), InitrdLayer: commit.Boot.InitrdLayer.String(), KernelFile: commit.Boot.KernelFile, InitrdFile: commit.Boot.InitrdFile,
			Size: commit.Size, CreatedAt: commit.Created,
		}
		if err := putJSON(ctx, writer, CollectionImages, commit.Manifest.Digest.String(), record); err != nil {
			return err
		}
		for position, layer := range commit.Layers {
			record := layerRecord{
				ManifestDigest: commit.Manifest.Digest.String(), Position: position,
				SourceDigest: layer.SourceDigest.String(), EROFSDigest: layer.EROFSDigest.String(),
				Size: layer.Size, BootFiles: encodeBootFiles(layer.BootFiles), Whiteouts: layer.Whiteouts, BootOpaque: layer.BootOpaque,
			}
			if err := putJSON(ctx, writer, CollectionLayers, layerKey(commit.Manifest.Digest, position), record); err != nil {
				return err
			}
		}
		return putJSON(ctx, writer, CollectionNames, commit.Name, nameRecord{ManifestDigest: commit.Manifest.Digest.String()})
	})
	return errdefs.Context(err, "commit image import", commit.Name, "metadata", "retry the import", false)
}

// Remove deletes one exact alias, or all aliases for a digest reference.
// expected protects against a reference rebound while the caller waited for locks.
// The final alias removal drops manifest rows and returns unreferenced source layers;
// the caller, holding the corresponding artifact locks, performs file cleanup.
func (c *Store) Remove(ctx context.Context, reference string, expected types.Digest) (images.Removal, error) {
	var result images.Removal
	// The transaction is the reachability boundary for shared artifacts:
	//
	//   remove requested names -> names remain? --yes--> retain image and layers
	//                                  |
	//                                  no
	//                                  v
	//                     remove image/layer rows -> find unreferenced digests
	err := c.store.Update(ctx, func(writer metadata.Writer) error {
		result = images.Removal{}
		image, err := resolveRecord(ctx, writer, reference)
		if err != nil {
			return err
		}
		if image.ManifestDigest != expected {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeNameTaken, errors.New("image binding changed while waiting for locks; retry"))
		}
		digest := image.ManifestDigest.String()
		// Exact names take precedence over digest prefixes, including hex-looking names.
		_, isName, err := writer.Get(ctx, CollectionNames, reference)
		if err != nil {
			return err
		}
		removeAllNames := !isName
		for _, name := range image.Names {
			if removeAllNames || name == reference {
				if err := writer.Delete(ctx, CollectionNames, name); err != nil {
					return err
				}
				result.Names = append(result.Names, name)
			}
		}
		remaining := 0
		if err := writer.Scan(ctx, CollectionNames, func(_ string, raw []byte) error {
			var record nameRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return corruptRecord("name", err)
			}
			if record.ManifestDigest == digest {
				remaining++
			}
			return nil
		}); err != nil {
			return err
		}
		if remaining > 0 {
			return nil
		}
		if c.usage != nil {
			used, err := c.usage.InUse(ctx, writer, image.ManifestDigest)
			if err != nil {
				return err
			}
			if used {
				return errdefs.New(errdefs.ClassConflict, errdefs.CodeReferenced, fmt.Errorf("image %s is used by a sandbox", image.ManifestDigest))
			}
		}
		if err := writer.Delete(ctx, CollectionImages, digest); err != nil {
			return err
		}
		for position, layer := range image.Layers {
			if err := writer.Delete(ctx, CollectionLayers, layerKey(image.ManifestDigest, position)); err != nil {
				return err
			}
			used, err := layerReferenced(ctx, writer, layer.SourceDigest)
			if err != nil {
				return err
			}
			if !used {
				result.Layers = append(result.Layers, layer.SourceDigest)
			}
		}
		return nil
	})
	return result, errdefs.Context(err, "remove image", reference, "metadata", "inspect image references", false)
}

// resolveRecord keeps alias precedence consistent between lookup and removal,
// so even a hex-looking exact alias never accidentally selects a different image.
func resolveRecord(ctx context.Context, reader metadata.Reader, reference string) (types.Image, error) {
	digestID := ""
	if raw, ok, err := reader.Get(ctx, CollectionNames, reference); err != nil {
		return types.Image{}, err
	} else if ok {
		var record nameRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return types.Image{}, corruptRecord("name", err)
		}
		digestID = record.ManifestDigest
	} else {
		prefix := strings.TrimPrefix(reference, "sha256:")
		if len(prefix) < minimumDigestPrefix {
			return types.Image{}, errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, fmt.Errorf("image %q not found", reference))
		}
		if err := reader.Scan(ctx, CollectionImages, func(id string, _ []byte) error {
			if strings.HasPrefix(strings.TrimPrefix(id, "sha256:"), prefix) {
				if digestID != "" && digestID != id {
					return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("digest prefix %q is ambiguous", reference))
				}
				digestID = id
			}
			return nil
		}); err != nil {
			return types.Image{}, err
		}
	}
	if digestID == "" {
		return types.Image{}, errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, fmt.Errorf("image %q not found", reference))
	}
	return loadImage(ctx, reader, digestID)
}

// loadImage reconstructs and validates records within the caller's transaction.
// Identity, contiguous positions and derived boot/size facts must agree before
// persisted data can be exposed as a domain image.
func loadImage(ctx context.Context, reader metadata.Reader, digestID string) (types.Image, error) {
	raw, ok, err := reader.Get(ctx, CollectionImages, digestID)
	if err != nil {
		return types.Image{}, err
	}
	if !ok {
		return types.Image{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("image record %s is missing", digestID))
	}
	var record imageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return types.Image{}, corruptRecord("image", err)
	}
	manifest, err := types.ParseDigest(record.ManifestDigest)
	if err != nil {
		return types.Image{}, corruptRecord("image digest", err)
	}
	kernel, err := types.ParseDigest(record.KernelLayer)
	if err != nil {
		return types.Image{}, corruptRecord("kernel digest", err)
	}
	initrd, err := types.ParseDigest(record.InitrdLayer)
	if err != nil {
		return types.Image{}, corruptRecord("initrd digest", err)
	}
	image := types.Image{
		ManifestDigest: manifest, Platform: types.Platform{OS: record.OS, Architecture: record.Architecture},
		Boot: types.Boot{Profile: types.BootProfile(record.BootProfile), KernelLayer: kernel, InitrdLayer: initrd, KernelFile: record.KernelFile, InitrdFile: record.InitrdFile}, Size: record.Size, CreatedAt: record.CreatedAt,
	}
	if err := reader.Scan(ctx, CollectionNames, func(name string, raw []byte) error {
		var item nameRecord
		if err := json.Unmarshal(raw, &item); err != nil {
			return corruptRecord("name", err)
		}
		if item.ManifestDigest == digestID {
			image.Names = append(image.Names, name)
		}
		return nil
	}); err != nil {
		return types.Image{}, err
	}
	var layerRecords []layerRecord
	if err := reader.Scan(ctx, CollectionLayers, func(key string, raw []byte) error {
		var item layerRecord
		if err := json.Unmarshal(raw, &item); err != nil {
			return corruptRecord("layer", err)
		}
		if item.ManifestDigest != digestID {
			return nil
		}
		if item.Position < 0 || key != layerKey(manifest, item.Position) {
			return corruptRecord("layer position", errors.New("invalid layer key or position"))
		}
		layerRecords = append(layerRecords, item)
		return nil
	}); err != nil {
		return types.Image{}, err
	}
	slices.SortFunc(layerRecords, func(a, b layerRecord) int { return a.Position - b.Position })
	for pos, item := range layerRecords {
		if item.Position != pos {
			return types.Image{}, corruptRecord("layer order", errors.New("noncontiguous layer positions"))
		}
		layer, err := decodeLayer(item)
		if err != nil {
			return types.Image{}, err
		}
		image.Layers = append(image.Layers, layer)
	}
	if manifest.String() != digestID {
		return types.Image{}, corruptRecord("image identity", errors.New("record key differs from manifest digest"))
	}
	descriptors := make([]types.Descriptor, len(image.Layers))
	for pos, layer := range image.Layers {
		descriptors[pos] = types.Descriptor{Digest: layer.SourceDigest}
	}
	if err := (images.ImportCommit{Name: "stored", Manifest: types.Manifest{Digest: manifest, Platform: image.Platform, BootProfile: image.Boot.Profile, Layers: descriptors}, Layers: image.Layers, Boot: image.Boot, Size: image.Size, Created: image.CreatedAt}).Validate(); err != nil {
		return types.Image{}, corruptRecord("image facts", err)
	}
	slices.Sort(image.Names)
	return image, nil
}

// decodeLayer validates serialized artifact identities and boot overlay facts.
// It verifies metadata shape only; images.Verify checks the actual files.
func decodeLayer(record layerRecord) (types.Layer, error) {
	source, err := types.ParseDigest(record.SourceDigest)
	if err != nil {
		return types.Layer{}, corruptRecord("source layer digest", err)
	}
	erofs, err := types.ParseDigest(record.EROFSDigest)
	if err != nil {
		return types.Layer{}, corruptRecord("erofs digest", err)
	}
	layer := types.Layer{SourceDigest: source, EROFSDigest: erofs, Size: record.Size, Whiteouts: record.Whiteouts, BootOpaque: record.BootOpaque}
	for _, file := range record.BootFiles {
		digest, err := types.ParseDigest(file.Digest)
		if err != nil || !images.IsBootName(file.Name) || file.Size <= 0 {
			return types.Layer{}, corruptRecord("boot file", errors.New("invalid name, digest or size"))
		}
		layer.BootFiles = append(layer.BootFiles, types.BootFile{Name: file.Name, Digest: digest, Size: file.Size})
	}
	if layer.SourceDigest.IsZero() || layer.EROFSDigest.IsZero() || layer.Size <= 0 {
		return types.Layer{}, corruptRecord("layer", errors.New("invalid digest or size"))
	}
	for _, name := range layer.Whiteouts {
		if !images.IsBootName(name) {
			return types.Layer{}, corruptRecord("whiteout", errors.New("invalid boot whiteout"))
		}
	}
	return layer, nil
}

func putJSON(ctx context.Context, writer metadata.Writer, collection metadata.Collection, id string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s record %q: %w", collection, id, err)
	}
	return writer.Put(ctx, collection, id, raw)
}

func corruptRecord(kind string, cause error) error {
	return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("decode %s metadata: %w", kind, cause))
}

// layerKey preserves distinct repeated source layers by using manifest position.
func layerKey(manifest types.Digest, position int) string {
	return fmt.Sprintf("%s/%08d", manifest.String(), position)
}

// layerReferenced checks remaining occurrences in the current write transaction
// before authorizing filesystem reclamation of a shared source digest.
func layerReferenced(ctx context.Context, reader metadata.Reader, digest types.Digest) (bool, error) {
	referenced := false
	err := reader.Scan(ctx, CollectionLayers, func(_ string, raw []byte) error {
		var record layerRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return corruptRecord("layer", err)
		}
		if record.SourceDigest == digest.String() {
			referenced = true
		}
		return nil
	})
	return referenced, err
}
