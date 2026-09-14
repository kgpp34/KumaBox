package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/metadata"
)

const (
	CollectionImages    metadata.Collection = "images"
	CollectionNames     metadata.Collection = "image_names"
	CollectionLayers    metadata.Collection = "image_layers"
	minimumDigestPrefix                     = 12
)

func Collections() []metadata.Collection {
	return []metadata.Collection{CollectionImages, CollectionNames, CollectionLayers}
}

type MetadataCatalog struct {
	store metadata.Store
}

func NewMetadataCatalog(store metadata.Store) *MetadataCatalog {
	return &MetadataCatalog{store: store}
}

type imageRecord struct {
	ManifestDigest string    `json:"manifest_digest"`
	OS             string    `json:"os"`
	Architecture   string    `json:"architecture"`
	KernelLayer    string    `json:"kernel_layer"`
	KernelFile     string    `json:"kernel_file"`
	InitrdFile     string    `json:"initrd_file"`
	InitrdLayer    string    `json:"initrd_layer"`
	Size           int64     `json:"size"`
	CreatedAt      time.Time `json:"created_at"`
}

type nameRecord struct {
	ManifestDigest string `json:"manifest_digest"`
}

type layerRecord struct {
	ManifestDigest string           `json:"manifest_digest"`
	Position       int              `json:"position"`
	SourceDigest   string           `json:"source_digest"`
	EROFSDigest    string           `json:"erofs_digest"`
	Size           int64            `json:"size"`
	BootFiles      []bootFileRecord `json:"boot_files"`
	Whiteouts      []string         `json:"whiteouts"`
	BootOpaque     bool             `json:"boot_opaque"`
}

type bootFileRecord struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

func encodeBootFiles(files []BootFile) []bootFileRecord {
	result := make([]bootFileRecord, 0, len(files))
	for _, file := range files {
		result = append(result, bootFileRecord{Name: file.Name, Digest: file.Digest.String(), Size: file.Size})
	}
	return result
}

func (c *MetadataCatalog) Resolve(ctx context.Context, reference string) (Image, error) {
	var result Image
	err := c.store.View(ctx, func(reader metadata.Reader) error {
		image, err := resolveRecord(ctx, reader, reference)
		if err != nil {
			return err
		}
		result = image
		return nil
	})
	return result, errdefs.Context(err, "resolve image", reference, "metadata", "check the image name or digest", false)
}

func (c *MetadataCatalog) List(ctx context.Context) ([]Image, error) {
	result := make([]Image, 0)
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
	slices.SortFunc(result, func(left, right Image) int {
		return strings.Compare(left.ManifestDigest.String(), right.ManifestDigest.String())
	})
	return result, errdefs.Context(err, "list images", "", "metadata", "inspect the metadata store", false)
}

func (c *MetadataCatalog) FindLayers(ctx context.Context, digests []Digest) (map[Digest]Layer, error) {
	wanted := make(map[Digest]struct{}, len(digests))
	for _, digest := range digests {
		wanted[digest] = struct{}{}
	}
	result := make(map[Digest]Layer)
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
				if previous, ok := result[layer.SourceDigest]; ok && !sameLayer(previous, layer) {
					return corruptRecord("shared layer", errors.New("conflicting artifact metadata"))
				}
				result[layer.SourceDigest] = layer
			}
			return nil
		})
	})
	return result, err
}

func (c *MetadataCatalog) CommitImport(ctx context.Context, commit ImportCommit) error {
	if err := validateCommit(commit); err != nil {
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
				if !sameLayer(layer, commit.Layers[pos]) {
					return corruptRecord("image", errors.New("manifest layers changed"))
				}
			}
			return putJSON(ctx, writer, CollectionNames, commit.Name, nameRecord{ManifestDigest: commit.Manifest.Digest.String()})
		}
		record := imageRecord{
			ManifestDigest: commit.Manifest.Digest.String(), OS: commit.Manifest.Platform.OS,
			Architecture: commit.Manifest.Platform.Architecture,
			KernelLayer:  commit.Boot.KernelLayer.String(), InitrdLayer: commit.Boot.InitrdLayer.String(), KernelFile: commit.Boot.KernelFile, InitrdFile: commit.Boot.InitrdFile,
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

func (c *MetadataCatalog) Remove(ctx context.Context, reference string, expected Digest) (Removal, error) {
	var result Removal
	err := c.store.Update(ctx, func(writer metadata.Writer) error {
		result = Removal{}
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

func resolveRecord(ctx context.Context, reader metadata.Reader, reference string) (Image, error) {
	digestID := ""
	if raw, ok, err := reader.Get(ctx, CollectionNames, reference); err != nil {
		return Image{}, err
	} else if ok {
		var record nameRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return Image{}, corruptRecord("name", err)
		}
		digestID = record.ManifestDigest
	} else {
		prefix := strings.TrimPrefix(reference, "sha256:")
		if len(prefix) < minimumDigestPrefix {
			return Image{}, errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, fmt.Errorf("image %q not found", reference))
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
			return Image{}, err
		}
	}
	if digestID == "" {
		return Image{}, errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, fmt.Errorf("image %q not found", reference))
	}
	return loadImage(ctx, reader, digestID)
}

func loadImage(ctx context.Context, reader metadata.Reader, digestID string) (Image, error) {
	raw, ok, err := reader.Get(ctx, CollectionImages, digestID)
	if err != nil {
		return Image{}, err
	}
	if !ok {
		return Image{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("image record %s is missing", digestID))
	}
	var record imageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return Image{}, corruptRecord("image", err)
	}
	manifest, err := ParseDigest(record.ManifestDigest)
	if err != nil {
		return Image{}, corruptRecord("image digest", err)
	}
	kernel, err := ParseDigest(record.KernelLayer)
	if err != nil {
		return Image{}, corruptRecord("kernel digest", err)
	}
	initrd, err := ParseDigest(record.InitrdLayer)
	if err != nil {
		return Image{}, corruptRecord("initrd digest", err)
	}
	image := Image{
		ManifestDigest: manifest, Platform: Platform{OS: record.OS, Architecture: record.Architecture},
		Boot: Boot{KernelLayer: kernel, InitrdLayer: initrd, KernelFile: record.KernelFile, InitrdFile: record.InitrdFile}, Size: record.Size, CreatedAt: record.CreatedAt,
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
		return Image{}, err
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
		return Image{}, err
	}
	slices.SortFunc(layerRecords, func(a, b layerRecord) int { return a.Position - b.Position })
	for pos, item := range layerRecords {
		if item.Position != pos {
			return Image{}, corruptRecord("layer order", errors.New("noncontiguous layer positions"))
		}
		layer, err := decodeLayer(item)
		if err != nil {
			return Image{}, err
		}
		image.Layers = append(image.Layers, layer)
	}
	if manifest.String() != digestID {
		return Image{}, corruptRecord("image identity", errors.New("record key differs from manifest digest"))
	}
	descriptors := make([]Descriptor, len(image.Layers))
	for pos, layer := range image.Layers {
		descriptors[pos] = Descriptor{Digest: layer.SourceDigest}
	}
	if err := validateCommit(ImportCommit{Name: "stored", Manifest: Manifest{Digest: manifest, Platform: image.Platform, Layers: descriptors}, Layers: image.Layers, Boot: image.Boot, Size: image.Size, Created: image.CreatedAt}); err != nil {
		return Image{}, corruptRecord("image facts", err)
	}
	slices.Sort(image.Names)
	return image, nil
}

func decodeLayer(record layerRecord) (Layer, error) {
	source, err := ParseDigest(record.SourceDigest)
	if err != nil {
		return Layer{}, corruptRecord("source layer digest", err)
	}
	erofs, err := ParseDigest(record.EROFSDigest)
	if err != nil {
		return Layer{}, corruptRecord("erofs digest", err)
	}
	layer := Layer{SourceDigest: source, EROFSDigest: erofs, Size: record.Size, Whiteouts: record.Whiteouts, BootOpaque: record.BootOpaque}
	for _, file := range record.BootFiles {
		digest, err := ParseDigest(file.Digest)
		if err != nil || !IsBootName(file.Name) || file.Size <= 0 {
			return Layer{}, corruptRecord("boot file", errors.New("invalid name, digest or size"))
		}
		layer.BootFiles = append(layer.BootFiles, BootFile{Name: file.Name, Digest: digest, Size: file.Size})
	}
	if layer.SourceDigest.IsZero() || layer.EROFSDigest.IsZero() || layer.Size <= 0 {
		return Layer{}, corruptRecord("layer", errors.New("invalid digest or size"))
	}
	for _, name := range layer.Whiteouts {
		if !IsBootName(name) {
			return Layer{}, corruptRecord("whiteout", errors.New("invalid boot whiteout"))
		}
	}
	return layer, nil
}

func validateCommit(commit ImportCommit) error {
	if commit.Name == "" || commit.Manifest.Digest.IsZero() || !validPlatform(commit.Manifest.Platform) || len(commit.Layers) == 0 || len(commit.Layers) != len(commit.Manifest.Layers) || commit.Created.IsZero() {
		return errors.New("invalid image name, manifest, platform, layers or creation time")
	}
	var size int64
	for pos, layer := range commit.Layers {
		if layer.SourceDigest.IsZero() || layer.EROFSDigest.IsZero() || layer.SourceDigest != commit.Manifest.Layers[pos].Digest || layer.Size <= 0 || size > (1<<63-1)-layer.Size {
			return errors.New("invalid layer identity, order or size")
		}
		size += layer.Size
		seen := make(map[string]bool)
		for _, file := range layer.BootFiles {
			if !IsBootName(file.Name) || file.Digest.IsZero() || file.Size <= 0 || seen[file.Name] {
				return errors.New("invalid boot file metadata")
			}
			seen[file.Name] = true
		}
		for _, name := range layer.Whiteouts {
			if !IsBootName(name) {
				return errors.New("invalid boot whiteout")
			}
		}
	}
	boot, err := selectBoot(commit.Layers)
	if err != nil || boot != commit.Boot || size != commit.Size {
		return errors.New("inconsistent boot selection or total image size")
	}
	return nil
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

func layerKey(manifest Digest, position int) string {
	return fmt.Sprintf("%s/%08d", manifest.String(), position)
}

func layerReferenced(ctx context.Context, reader metadata.Reader, digest Digest) (bool, error) {
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
