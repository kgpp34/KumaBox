package oci

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
)

const maxMetadataSize = 16 << 20

type Limits = images.Limits

func DefaultLimits() Limits { return images.DefaultLimits() }

type resolvedLayer struct {
	layer     v1.Layer
	diffID    images.Digest
	mediaType types.MediaType
}

type resolvedSource struct {
	resolve func(context.Context, images.Platform) (v1.Image, error)
	mu      sync.RWMutex
	layers  map[images.Digest]resolvedLayer
	limits  Limits
}

func (s *resolvedSource) Resolve(ctx context.Context, platform images.Platform) (images.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return images.Manifest{}, err
	}
	image, err := s.resolve(ctx, platform)
	if err != nil {
		return images.Manifest{}, sourceError(err)
	}
	raw, err := image.RawManifest()
	if err != nil {
		return images.Manifest{}, sourceError(err)
	}
	if len(raw) > maxMetadataSize {
		return images.Manifest{}, invalidSource("manifest exceeds metadata limit")
	}
	manifest, err := v1.ParseManifest(bytes.NewReader(raw))
	if err != nil {
		return images.Manifest{}, invalidSource("invalid OCI manifest: %v", err)
	}
	manifestHash, err := image.Digest()
	if err != nil {
		return images.Manifest{}, sourceError(err)
	}
	if err := checkBytes(raw, manifestHash, int64(len(raw))); err != nil {
		return images.Manifest{}, err
	}
	digest, err := images.ParseDigest(manifestHash.String())
	if err != nil {
		return images.Manifest{}, invalidSource("invalid manifest digest: %v", err)
	}
	if manifest.SchemaVersion != 2 {
		return images.Manifest{}, invalidSource("unsupported OCI manifest schema version")
	}
	if manifest.Config.MediaType != types.OCIConfigJSON && manifest.Config.MediaType != types.DockerConfigJSON {
		return images.Manifest{}, invalidSource("unsupported OCI config media type")
	}
	if err := validateDescriptor(manifest.Config, maxMetadataSize); err != nil {
		return images.Manifest{}, err
	}
	configRaw, err := image.RawConfigFile()
	if err != nil {
		return images.Manifest{}, sourceError(err)
	}
	if err := checkBytes(configRaw, manifest.Config.Digest, manifest.Config.Size); err != nil {
		return images.Manifest{}, err
	}
	config, err := v1.ParseConfigFile(bytes.NewReader(configRaw))
	if err != nil {
		return images.Manifest{}, invalidSource("invalid OCI config: %v", err)
	}
	if config.OS != platform.OS || config.Architecture != platform.Architecture {
		return images.Manifest{}, invalidSource("image platform %s/%s does not match %s/%s", config.OS, config.Architecture, platform.OS, platform.Architecture)
	}
	if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return images.Manifest{}, invalidSource("config rootfs does not match manifest layers")
	}
	layers := make(map[images.Digest]resolvedLayer)
	descriptors := make([]images.Descriptor, len(manifest.Layers))
	for position, desc := range manifest.Layers {
		if err := validateDescriptor(desc, s.limits.LayerSize); err != nil {
			return images.Manifest{}, err
		}
		switch desc.MediaType {
		case types.OCILayer, types.OCIUncompressedLayer, types.OCILayerZStd, types.DockerLayer, types.DockerUncompressedLayer:
		default:
			return images.Manifest{}, invalidSource("unsupported layer media type %s", desc.MediaType)
		}
		digest, err := images.ParseDigest(desc.Digest.String())
		if err != nil {
			return images.Manifest{}, invalidSource("invalid layer digest: %v", err)
		}
		diffID, err := images.ParseDigest(config.RootFS.DiffIDs[position].String())
		if err != nil {
			return images.Manifest{}, invalidSource("invalid layer diffID: %v", err)
		}
		layer, err := image.LayerByDigest(desc.Digest)
		if err != nil {
			return images.Manifest{}, sourceError(err)
		}
		if existing, ok := layers[digest]; ok && existing.diffID != diffID {
			return images.Manifest{}, invalidSource("repeated layer has inconsistent diffID")
		}
		layers[digest] = resolvedLayer{layer: layer, diffID: diffID, mediaType: desc.MediaType}
		descriptors[position] = images.Descriptor{Digest: digest, Size: desc.Size}
	}
	s.mu.Lock()
	s.layers = layers
	s.mu.Unlock()
	return images.Manifest{Digest: digest, Platform: platform, Layers: descriptors}, nil
}

func (s *resolvedSource) OpenLayer(ctx context.Context, descriptor images.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	layer, ok := s.layers[descriptor.Digest]
	s.mu.RUnlock()
	if !ok {
		return nil, invalidSource("layer %s was not resolved", descriptor.Digest)
	}
	raw, err := layer.layer.Compressed()
	if err != nil {
		return nil, sourceError(err)
	}
	compressed := &checkedReader{ctx: ctx, reader: raw, hash: sha256.New(), expected: descriptor.Digest, limit: s.limits.LayerSize, size: descriptor.Size}
	buffered := bufio.NewReader(compressed)
	var input io.Reader = buffered
	closeDecoder := func() error { return nil }
	switch layer.mediaType {
	case types.OCILayer, types.DockerLayer:
		decoder, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, errors.Join(sourceError(err), raw.Close())
		}
		input, closeDecoder = decoder, decoder.Close
	case types.OCILayerZStd:
		decoder, err := zstd.NewReader(buffered, zstd.WithDecoderMaxMemory(uint64(max(1, s.limits.UnpackedSize))), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, errors.Join(sourceError(err), raw.Close())
		}
		input = decoder
		closeDecoder = func() error { decoder.Close(); return nil }
	}
	unpacked := &checkedReader{ctx: ctx, reader: input, hash: sha256.New(), expected: layer.diffID, limit: s.limits.UnpackedSize, size: -1}
	return &layerReader{unpacked: unpacked, buffered: buffered, raw: raw, closeDecoder: closeDecoder}, nil
}

type checkedReader struct {
	ctx      context.Context
	reader   io.Reader
	hash     hash.Hash
	expected images.Digest
	limit    int64
	size     int64
	read     int64
	lastErr  error
}

func (r *checkedReader) Read(p []byte) (n int, returnErr error) {
	if r.lastErr != nil {
		return 0, r.lastErr
	}
	defer func() {
		if returnErr != nil {
			r.lastErr = returnErr
		}
	}()
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > r.limit-r.read+1 {
		p = p[:r.limit-r.read+1]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if _, hashErr := r.hash.Write(p[:n]); hashErr != nil {
		return n, hashErr
	}
	if r.read > r.limit {
		return n, invalidSource("layer exceeds %d bytes", r.limit)
	}
	if errors.Is(err, io.EOF) {
		if fmt.Sprintf("sha256:%x", r.hash.Sum(nil)) != r.expected.String() || (r.size >= 0 && r.size != r.read) {
			return n, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, fmt.Errorf("layer %s digest or size mismatch", r.expected))
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		err = sourceError(err)
	}
	return n, err
}

type layerReader struct {
	unpacked     *checkedReader
	buffered     *bufio.Reader
	raw          io.ReadCloser
	closeDecoder func() error
}

func (r *layerReader) Read(p []byte) (int, error) {
	n, err := r.unpacked.Read(p)
	if errors.Is(err, io.EOF) {
		if _, drainErr := io.Copy(io.Discard, r.buffered); drainErr != nil {
			return n, drainErr
		}
	}
	return n, err
}
func (r *layerReader) Close() error { return errors.Join(r.closeDecoder(), r.raw.Close()) }

func validateDescriptor(desc v1.Descriptor, limit int64) error {
	if _, err := images.ParseDigest(desc.Digest.String()); err != nil {
		return invalidSource("invalid descriptor: %v", err)
	}
	if desc.Size < 0 || desc.Size > limit {
		return invalidSource("descriptor size %d exceeds limit %d", desc.Size, limit)
	}
	if len(desc.URLs) != 0 {
		return invalidSource("external descriptor URLs are unsupported")
	}
	return nil
}

func checkBytes(raw []byte, expected v1.Hash, size int64) error {
	if expected.Algorithm != "sha256" || fmt.Sprintf("%x", sha256.Sum256(raw)) != expected.Hex || int64(len(raw)) != size {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, fmt.Errorf("OCI object %s digest or size mismatch", expected))
	}
	return nil
}

func invalidSource(format string, args ...any) error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func sourceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) {
		return errdefs.New(errdefs.ClassCorrupt, errdefs.CodeDigestMismatch, err)
	}
	if _, ok := errdefs.CodeOf(err); ok {
		return err
	}
	return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
}
