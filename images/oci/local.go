package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/kumabox/kumabox/images"
)

// Local metadata is bounded before allocation; os.Root also contains concurrent path changes.
func readLocal(ctx context.Context, path, name string, limit int64) ([]byte, error) {
	reader, err := openLocal(ctx, path, name)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, invalidSource("OCI metadata exceeds %d bytes", limit)
	}
	return raw, nil
}

func openLocal(ctx context.Context, path, name string) (io.ReadCloser, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = invalidSource("OCI blob is not a regular file")
		}
		return nil, errors.Join(err, file.Close(), root.Close())
	}
	return &localReader{Reader: &contextInput{ctx: ctx, source: file}, file: file, root: root}, nil
}

type localReader struct {
	io.Reader
	file *os.File
	root *os.Root
}

func (r *localReader) Close() error { return errors.Join(r.file.Close(), r.root.Close()) }

func blobName(hash v1.Hash) (string, error) {
	digest, err := images.ParseDigest(hash.String())
	if err != nil {
		return "", invalidSource("invalid OCI blob digest: %v", err)
	}
	return "blobs/sha256/" + digest.Hex(), nil
}

type localIndex struct {
	path string
	raw  []byte
	ctx  context.Context
}

func (i *localIndex) MediaType() (types.MediaType, error) { return types.OCIImageIndex, nil }
func (i *localIndex) Digest() (v1.Hash, error)            { return partial.Digest(i) }
func (i *localIndex) Size() (int64, error)                { return int64(len(i.raw)), nil }
func (i *localIndex) RawManifest() ([]byte, error)        { return bytes.Clone(i.raw), nil }
func (i *localIndex) IndexManifest() (*v1.IndexManifest, error) {
	return v1.ParseIndexManifest(bytes.NewReader(i.raw))
}

func (i *localIndex) descriptor(hash v1.Hash) (v1.Descriptor, error) {
	manifest, err := i.IndexManifest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	for _, descriptor := range manifest.Manifests {
		if descriptor.Digest == hash {
			return descriptor, validateDescriptor(descriptor, maxMetadataSize)
		}
	}
	return v1.Descriptor{}, fmt.Errorf("OCI descriptor %s not found", hash)
}

func (i *localIndex) Image(hash v1.Hash) (v1.Image, error) {
	descriptor, err := i.descriptor(hash)
	if err != nil {
		return nil, err
	}
	name, err := blobName(hash)
	if err != nil {
		return nil, err
	}
	raw, err := readLocal(i.ctx, i.path, name, maxMetadataSize)
	if err != nil {
		return nil, err
	}
	if err := checkBytes(raw, descriptor.Digest, descriptor.Size); err != nil {
		return nil, err
	}
	return partial.CompressedToImage(&localImage{path: i.path, raw: raw, descriptor: descriptor, ctx: i.ctx})
}

func (i *localIndex) ImageIndex(hash v1.Hash) (v1.ImageIndex, error) {
	descriptor, err := i.descriptor(hash)
	if err != nil {
		return nil, err
	}
	name, err := blobName(hash)
	if err != nil {
		return nil, err
	}
	raw, err := readLocal(i.ctx, i.path, name, maxMetadataSize)
	if err != nil {
		return nil, err
	}
	if err := checkBytes(raw, descriptor.Digest, descriptor.Size); err != nil {
		return nil, err
	}
	return &localIndex{path: i.path, raw: raw, ctx: i.ctx}, nil
}

type localImage struct {
	path       string
	raw        []byte
	descriptor v1.Descriptor
	ctx        context.Context
}

func (i *localImage) MediaType() (types.MediaType, error) { return i.descriptor.MediaType, nil }
func (i *localImage) RawManifest() ([]byte, error)        { return bytes.Clone(i.raw), nil }
func (i *localImage) RawConfigFile() ([]byte, error) {
	manifest, err := v1.ParseManifest(bytes.NewReader(i.raw))
	if err != nil {
		return nil, err
	}
	if err := validateDescriptor(manifest.Config, maxMetadataSize); err != nil {
		return nil, err
	}
	name, err := blobName(manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	return readLocal(i.ctx, i.path, name, maxMetadataSize)
}

func (i *localImage) LayerByDigest(hash v1.Hash) (partial.CompressedLayer, error) {
	manifest, err := v1.ParseManifest(bytes.NewReader(i.raw))
	if err != nil {
		return nil, err
	}
	for _, descriptor := range manifest.Layers {
		if descriptor.Digest == hash {
			return &localLayer{path: i.path, descriptor: descriptor, ctx: i.ctx}, nil
		}
	}
	return nil, fmt.Errorf("OCI layer %s not found", hash)
}

type localLayer struct {
	path       string
	descriptor v1.Descriptor
	ctx        context.Context
}

func (l *localLayer) Digest() (v1.Hash, error)            { return l.descriptor.Digest, nil }
func (l *localLayer) Size() (int64, error)                { return l.descriptor.Size, nil }
func (l *localLayer) MediaType() (types.MediaType, error) { return l.descriptor.MediaType, nil }
func (l *localLayer) Compressed() (io.ReadCloser, error) {
	name, err := blobName(l.descriptor.Digest)
	if err != nil {
		return nil, err
	}
	return openLocal(l.ctx, l.path, name)
}
