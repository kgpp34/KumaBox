package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	mediatypes "github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/types"
)

func copyFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("../../testdata/oci-layout")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSourceChecksOCIObjects(t *testing.T) {
	for _, kind := range []string{"valid", "manifest", "config", "layer", "platform", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := copyFixture(t)
			indexSource, err := NewLayout(root)
			if err != nil {
				t.Fatal(err)
			}
			source := indexSource.(*resolvedSource)
			platform := types.Platform{OS: "linux", Architecture: "amd64"}
			resolved, err := source.resolve(t.Context(), platform)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := resolved.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			digest, err := resolved.Digest()
			if err != nil {
				t.Fatal(err)
			}
			var object string
			switch kind {
			case "manifest":
				object = digest.Hex
			case "config":
				object = manifest.Config.Digest.Hex
			case "layer":
				object = manifest.Layers[0].Digest.Hex
			case "platform":
				platform.Architecture = "arm64"
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
					t.Fatal(err)
				}
			}
			if object != "" {
				path := filepath.Join(root, "blobs", "sha256", object)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "layer" {
					raw[9] ^= 1
				} else {
					raw = append(raw, '\n')
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := source.Resolve(t.Context(), platform)
			if err == nil {
				reader, openErr := source.OpenLayer(t.Context(), got.Layers[0])
				err = openErr
				if openErr == nil {
					_, readErr := io.Copy(io.Discard, reader)
					err = errors.Join(readErr, reader.Close())
				}
			}
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			expected := errdefs.CodeDigestMismatch
			if kind == "platform" || kind == "symlink" {
				expected = errdefs.CodeInvalidArgument
			}
			if code, ok := errdefs.CodeOf(err); !ok || code != expected {
				t.Fatalf("error = %v, want %s", err, expected)
			}
		})
	}
}

func TestSourceBoundsDecompressionAndCancellation(t *testing.T) {
	limits := images.DefaultLimits()
	limits.UnpackedSize = 64
	source, err := NewLayoutWithLimits(copyFixture(t), limits)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := source.Resolve(t.Context(), types.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := source.OpenLayer(t.Context(), manifest.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, reader)
	err = errors.Join(readErr, reader.Close())
	if code, _ := errdefs.CodeOf(err); code != errdefs.CodeInvalidArgument {
		t.Fatalf("limit error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := source.OpenLayer(ctx, manifest.Layers[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestRegistryErrorsDoNotExposeCredentials(t *testing.T) {
	for _, reference := range []string{"https://user:secret@example.com/image", "user:secret@example.com/image"} {
		_, _, err := NewRegistry(reference)
		if err == nil || bytes.Contains([]byte(err.Error()), []byte("secret")) {
			t.Fatalf("unsafe parser error = %v", err)
		}
	}
	cause := errors.New("Authorization: Bearer secret")
	err := registryError(cause)
	if bytes.Contains([]byte(err.Error()), []byte("secret")) || !errors.Is(err, cause) {
		t.Fatalf("unsafe registry error = %v", err)
	}
}

func writeLayout(t *testing.T, compressed, unpacked []byte, media mediatypes.MediaType) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o750); err != nil {
		t.Fatal(err)
	}
	put := func(raw []byte, media mediatypes.MediaType) v1.Descriptor {
		digest := fmt.Sprintf("%x", sha256.Sum256(raw))
		if err := os.WriteFile(filepath.Join(root, "blobs", "sha256", digest), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return v1.Descriptor{Digest: v1.Hash{Algorithm: "sha256", Hex: digest}, Size: int64(len(raw)), MediaType: media}
	}
	encode := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	layer := put(compressed, media)
	config := put(encode(v1.ConfigFile{
		Architecture: "amd64", OS: "linux",
		RootFS: v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{{Algorithm: "sha256", Hex: fmt.Sprintf("%x", sha256.Sum256(unpacked))}}},
		Config: v1.Config{Labels: map[string]string{types.ImageBootProfileLabel: string(types.BootProfileOverlayV1)}},
	}), mediatypes.OCIConfigJSON)
	manifest := put(encode(v1.Manifest{SchemaVersion: 2, MediaType: mediatypes.OCIManifestSchema1, Config: config, Layers: []v1.Descriptor{layer}}), mediatypes.OCIManifestSchema1)
	if err := os.WriteFile(filepath.Join(root, "index.json"), encode(v1.IndexManifest{SchemaVersion: 2, MediaType: mediatypes.OCIImageIndex, Manifests: []v1.Descriptor{manifest}}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSourceSupportsLayerCompressionAndChecksDiffID(t *testing.T) {
	unpacked := bootTar(t, []*tar.Header{{Name: "boot/vmlinuz", Typeflag: tar.TypeReg, Size: 8}})
	for _, media := range []mediatypes.MediaType{mediatypes.OCIUncompressedLayer, mediatypes.OCILayer, mediatypes.OCILayerZStd} {
		t.Run(string(media), func(t *testing.T) {
			compressed := unpacked
			switch media {
			case mediatypes.OCILayer:
				var buffer bytes.Buffer
				writer := gzip.NewWriter(&buffer)
				if _, err := writer.Write(unpacked); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				compressed = buffer.Bytes()
			case mediatypes.OCILayerZStd:
				writer, err := zstd.NewWriter(nil)
				if err != nil {
					t.Fatal(err)
				}
				compressed = writer.EncodeAll(unpacked, nil)
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for _, valid := range []bool{true, false} {
				diff := unpacked
				if !valid {
					diff = []byte("incorrect diffID")
				}
				source, err := NewLayout(writeLayout(t, compressed, diff, media))
				if err != nil {
					t.Fatal(err)
				}
				manifest, err := source.Resolve(t.Context(), types.Platform{OS: "linux", Architecture: "amd64"})
				if err != nil {
					t.Fatal(err)
				}
				if manifest.BootProfile != types.BootProfileOverlayV1 {
					t.Fatalf("boot profile = %q", manifest.BootProfile)
				}
				reader, err := source.OpenLayer(t.Context(), manifest.Layers[0])
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := io.ReadAll(reader)
				err = errors.Join(readErr, reader.Close())
				if valid {
					if err != nil || !bytes.Equal(got, unpacked) {
						t.Fatalf("decoded layer differs: %v", err)
					}
				} else if code, _ := errdefs.CodeOf(err); code != errdefs.CodeDigestMismatch {
					t.Fatalf("wrong diffID accepted: %v", err)
				}
			}
		})
	}
}

func TestRegistrySourcePullsFromHTTPRegistry(t *testing.T) {
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer server.Close()
	ref, err := name.NewTag(strings.TrimPrefix(server.URL, "http://")+"/tiny:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := layout.FromPath("../../testdata/oci-layout")
	if err != nil {
		t.Fatal(err)
	}
	index, err := fixture.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	image, err := imageForPlatform(index, types.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, image, remote.WithContext(t.Context())); err != nil {
		t.Fatal(err)
	}
	source, normalized, err := NewRegistry(ref.String())
	if err != nil {
		t.Fatal(err)
	}
	if normalized != ref.String() {
		t.Fatalf("reference = %s", normalized)
	}
	manifest, err := source.Resolve(t.Context(), types.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := source.OpenLayer(t.Context(), manifest.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, reader)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		t.Fatal(err)
	}
	missing, _, err := NewRegistry(strings.TrimPrefix(server.URL, "http://") + "/absent:v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Resolve(t.Context(), manifest.Platform); err == nil {
		t.Fatal("missing registry image succeeded")
	} else if code, _ := errdefs.CodeOf(err); code != errdefs.CodeNotFound {
		t.Fatalf("missing image = %v", err)
	}
}
