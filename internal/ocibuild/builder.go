// SPDX-License-Identifier: MIT

// Package ocibuild builds KumaBox image manifests from OCI images.
package ocibuild

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/ocistore"
)

// BuildRequest describes an OCI image build.
type BuildRequest struct {
	Name      string
	Ref       string
	Platform  string
	Source    string
	MkfsEROFS string
}

// Builder converts OCI layers into shared EROFS blobs and publishes an image record.
type Builder struct {
	rootDir  string
	erofsDir string
	stageDir string
}

// New returns a Builder rooted under rootDir.
func New(rootDir string) *Builder {
	base := filepath.Join(rootDir, "oci", "erofs")
	return &Builder{
		rootDir:  rootDir,
		erofsDir: filepath.Join(base, "blobs"),
		stageDir: filepath.Join(base, "staging"),
	}
}

// Build pulls an OCI image, converts its layers to EROFS, and records image metadata.
func (b *Builder) Build(ctx context.Context, req BuildRequest) (*imagestore.ImageRecord, error) {
	if req.Name == "" {
		return nil, errors.New("image name must not be empty")
	}
	if req.Ref == "" {
		return nil, errors.New("OCI ref must not be empty")
	}
	if req.MkfsEROFS == "" {
		req.MkfsEROFS = "mkfs.erofs"
	}

	pull, err := ocistore.New(b.rootDir).Pull(ctx, ocistore.PullRequest{
		Ref:      req.Ref,
		Platform: req.Platform,
		Source:   req.Source,
	})
	if err != nil {
		return nil, err
	}

	layers := make([]imagestore.OCILayer, 0, len(pull.Layers))
	for i, layer := range pull.Layers {
		erofs, err := b.ensureEROFS(ctx, req.MkfsEROFS, layer)
		if err != nil {
			return nil, fmt.Errorf("build layer %d %s: %w", i, layer.Digest, err)
		}
		layers = append(layers, imagestore.OCILayer{
			Index:     i,
			Digest:    layer.Digest,
			MediaType: layer.MediaType,
			SizeBytes: layer.SizeBytes,
			EROFS:     erofs,
		})
	}

	return imagestore.New(b.rootDir).Create(imagestore.CreateRequest{
		Name: req.Name,
		Source: imagestore.Source{
			Type: "oci",
			URI:  pull.DigestRef,
		},
		OS: imagestore.OS{
			Family:  "linux",
			Profile: "oci-erofs",
		},
		OCI: &imagestore.OCI{
			Ref:       pull.Ref,
			Source:    pull.Source,
			DigestRef: pull.DigestRef,
			Platform: imagestore.OCIPlatform{
				OS:           pull.Platform.OS,
				Architecture: pull.Platform.Architecture,
				Variant:      pull.Platform.Variant,
			},
			Config: imagestore.OCIDescriptor{
				Digest:    pull.Config.Digest,
				MediaType: pull.Config.MediaType,
				SizeBytes: pull.Config.SizeBytes,
			},
			Layers:  layers,
			BuiltAt: time.Now().UTC(),
		},
	})
}

func (b *Builder) ensureEROFS(ctx context.Context, mkfs string, layer ocistore.BlobRecord) (*imagestore.EROFSLayer, error) {
	algo, value, err := splitDigest(layer.Digest)
	if err != nil {
		return nil, err
	}
	target := filepath.Join(b.erofsDir, algo, value+".erofs")
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		sum, err := fileSHA256(target)
		if err != nil {
			return nil, err
		}
		return &imagestore.EROFSLayer{
			Path:        target,
			Filesystem:  "erofs",
			Digest:      "sha256:" + sum,
			SizeBytes:   info.Size(),
			SourceLayer: layer.Digest,
		}, nil
	}

	opID, err := operationID()
	if err != nil {
		return nil, err
	}
	stage := filepath.Join(b.stageDir, "erofs-"+opID)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return nil, fmt.Errorf("create EROFS staging dir: %w", err)
	}
	defer os.RemoveAll(stage) //nolint:errcheck

	tarPath := filepath.Join(stage, "layer.tar")
	if err := writeLayerTar(layer, tarPath); err != nil {
		return nil, err
	}
	stagedEROFS := filepath.Join(stage, "layer.erofs")
	cmd := exec.CommandContext(ctx, mkfs, "--tar=f", stagedEROFS, tarPath) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("mkfs.erofs failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, fmt.Errorf("create EROFS blob dir: %w", err)
	}
	if err := os.Rename(stagedEROFS, target); err != nil {
		return nil, fmt.Errorf("commit EROFS blob: %w", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("stat EROFS blob: %w", err)
	}
	sum, err := fileSHA256(target)
	if err != nil {
		return nil, err
	}
	return &imagestore.EROFSLayer{
		Path:        target,
		Filesystem:  "erofs",
		Digest:      "sha256:" + sum,
		SizeBytes:   info.Size(),
		SourceLayer: layer.Digest,
	}, nil
}

func writeLayerTar(layer ocistore.BlobRecord, dst string) error {
	in, err := os.Open(layer.Path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open layer blob: %w", err)
	}
	defer in.Close() //nolint:errcheck

	reader, closeReader, err := layerTarReader(layer.MediaType, in)
	if err != nil {
		return err
	}
	defer closeReader()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create layer tar staging file: %w", err)
	}
	_, copyErr := io.Copy(out, reader)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("write layer tar staging file: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close layer tar staging file: %w", closeErr)
	}
	return nil
}

func layerTarReader(mediaType string, src io.Reader) (io.Reader, func(), error) {
	switch {
	case strings.HasSuffix(mediaType, ".tar+gzip"), strings.HasSuffix(mediaType, ".tar.gzip"):
		reader, err := gzip.NewReader(src)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open gzip layer: %w", err)
		}
		return reader, func() { _ = reader.Close() }, nil
	case strings.HasSuffix(mediaType, ".tar+zstd"), strings.HasSuffix(mediaType, ".tar.zstd"):
		reader, err := zstd.NewReader(src)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open zstd layer: %w", err)
		}
		return reader, reader.Close, nil
	case strings.HasSuffix(mediaType, ".tar"):
		return src, func() {}, nil
	default:
		return nil, func() {}, fmt.Errorf("OCI_LAYER_MEDIA_TYPE_UNSUPPORTED: %s", mediaType)
	}
}

func splitDigest(digest string) (string, string, error) {
	algo, value, ok := strings.Cut(digest, ":")
	if !ok || algo != "sha256" || len(value) != sha256.Size*2 {
		return "", "", fmt.Errorf("OCI_DIGEST_INVALID: %s", digest)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", "", fmt.Errorf("OCI_DIGEST_INVALID: %w", err)
	}
	return algo, value, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("open file for sha256: %w", err)
	}
	defer file.Close() //nolint:errcheck

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("hash file: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func operationID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate operation ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
