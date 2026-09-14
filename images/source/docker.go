package source

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
)

type dockerEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

func newDockerSource(path string, options LocalOptions) (images.Source, error) {
	if options.SourceTag != "" {
		tag, err := name.NewTag(options.SourceTag)
		if err != nil {
			return nil, invalidSource("invalid Docker source tag %q", options.SourceTag)
		}
		options.SourceTag = tag.Name()
	}
	source := &resolvedSource{limits: options.Limits}
	source.resolve = func(ctx context.Context, platform images.Platform) (v1.Image, error) {
		entry, config, err := selectDockerEntry(ctx, path, platform, options.SourceTag)
		if err != nil {
			return nil, err
		}
		return dockerImageFromEntry(ctx, path, entry, config, options.Limits)
	}
	return source, nil
}

func selectDockerEntry(ctx context.Context, path string, platform images.Platform, sourceTag string) (dockerEntry, []byte, error) {
	raw, err := readLocal(ctx, path, "manifest.json", maxMetadataSize)
	if err != nil {
		return dockerEntry{}, nil, err
	}
	var entries []dockerEntry
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return dockerEntry{}, nil, invalidSource("invalid docker save manifest.json")
	}
	var selected dockerEntry
	var selectedConfig []byte
	var count int
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return dockerEntry{}, nil, err
		}
		if sourceTag != "" && !dockerTagMatches(entry.RepoTags, sourceTag) {
			continue
		}
		config, err := readDockerConfig(ctx, path, entry.Config)
		if err != nil {
			return dockerEntry{}, nil, err
		}
		parsed, err := v1.ParseConfigFile(bytes.NewReader(config))
		if err != nil {
			return dockerEntry{}, nil, invalidSource("invalid Docker image config: %v", err)
		}
		if parsed.OS != platform.OS || parsed.Architecture != platform.Architecture {
			continue
		}
		if parsed.RootFS.Type != "layers" || len(parsed.RootFS.DiffIDs) != len(entry.Layers) {
			return dockerEntry{}, nil, invalidSource("Docker config rootfs does not match archive layers")
		}
		count++
		selected, selectedConfig = entry, config
	}
	if count == 0 {
		return dockerEntry{}, nil, invalidSource("no Docker image matches platform %s/%s and source tag %q", platform.OS, platform.Architecture, sourceTag)
	}
	if count != 1 {
		return dockerEntry{}, nil, invalidSource("Docker archive has %d matching images; select one with --source-tag", count)
	}
	return selected, selectedConfig, nil
}

func dockerTagMatches(tags []string, wanted string) bool {
	for _, value := range tags {
		tag, err := name.NewTag(value)
		if err == nil && tag.Name() == wanted {
			return true
		}
	}
	return false
}

func archiveObjectName(value string) (string, error) {
	clean := filepath.Clean(value)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", invalidSource("unsafe image archive object path %q", value)
	}
	return clean, nil
}

func readDockerConfig(ctx context.Context, path, object string) ([]byte, error) {
	object, err := archiveObjectName(object)
	if err != nil {
		return nil, err
	}
	// docker save names configs by their content hash (legacy .json names,
	// sha256:hex names, or modern blobs/sha256/hex paths).
	hex := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(object), ".json"), "sha256:")
	digest, err := images.ParseDigest("sha256:" + hex)
	if err != nil {
		return nil, invalidSource("Docker config filename must contain its sha256 digest")
	}
	raw, err := readLocal(ctx, path, object, maxMetadataSize)
	if err != nil {
		return nil, err
	}
	if err := checkBytes(raw, v1.Hash{Algorithm: "sha256", Hex: digest.Hex()}, int64(len(raw))); err != nil {
		return nil, err
	}
	return raw, nil
}

// Docker archives do not retain a registry manifest. Build a deterministic
// manifest from the config and ordered layer descriptors, then reuse the same
// digest/diffID validation and streaming as every other resolvedSource.
func dockerImageFromEntry(ctx context.Context, path string, entry dockerEntry, config []byte, limits images.Limits) (v1.Image, error) {
	configHash := v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", sha256.Sum256(config))}
	manifest := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: configHash, Size: int64(len(config))},
		Layers:        make([]v1.Descriptor, 0, len(entry.Layers)),
	}
	layers := make(map[v1.Hash]*fileLayer, len(entry.Layers))
	for _, object := range entry.Layers {
		object, err := archiveObjectName(object)
		if err != nil {
			return nil, err
		}
		descriptor, err := describeArchiveLayer(ctx, path, object, limits.LayerSize)
		if err != nil {
			return nil, err
		}
		manifest.Layers = append(manifest.Layers, descriptor)
		layers[descriptor.Digest] = &fileLayer{ctx: ctx, path: path, object: object, descriptor: descriptor}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return partial.CompressedToImage(&dockerImage{config: config, manifest: raw, layers: layers})
}

func describeArchiveLayer(ctx context.Context, path, object string, limit int64) (v1.Descriptor, error) {
	reader, err := openLocal(ctx, path, object)
	if err != nil {
		return v1.Descriptor{}, err
	}
	buffered := bufio.NewReader(reader)
	magic, peekErr := buffered.Peek(4)
	media := types.OCIUncompressedLayer
	if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		media = types.OCILayer
	} else if len(magic) == 4 && bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		media = types.OCILayerZStd
	}
	if peekErr != nil && !errors.Is(peekErr, io.EOF) {
		return v1.Descriptor{}, errors.Join(peekErr, reader.Close())
	}
	digest, size, readErr := v1.SHA256(io.LimitReader(buffered, limit+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return v1.Descriptor{}, err
	}
	if size > limit {
		return v1.Descriptor{}, invalidSource("Docker layer exceeds %d bytes", limit)
	}
	if strings.HasPrefix(filepath.ToSlash(object), "blobs/sha256/") && filepath.Base(object) != digest.Hex {
		return v1.Descriptor{}, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, errors.New("docker layer blob digest mismatch"))
	}
	return v1.Descriptor{MediaType: media, Digest: digest, Size: size}, nil
}

type dockerImage struct {
	config   []byte
	manifest []byte
	layers   map[v1.Hash]*fileLayer
}

var _ partial.CompressedImageCore = (*dockerImage)(nil)

func (i *dockerImage) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }
func (i *dockerImage) RawConfigFile() ([]byte, error)      { return bytes.Clone(i.config), nil }
func (i *dockerImage) RawManifest() ([]byte, error)        { return bytes.Clone(i.manifest), nil }
func (i *dockerImage) LayerByDigest(hash v1.Hash) (partial.CompressedLayer, error) {
	layer, ok := i.layers[hash]
	if !ok {
		return nil, invalidSource("Docker layer %s not found", hash)
	}
	return layer, nil
}
