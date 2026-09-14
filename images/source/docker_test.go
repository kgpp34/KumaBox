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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/klauspost/compress/zstd"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
)

func encodeJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fixtureDockerEntry(t *testing.T, architecture, tag, compression string) (dockerEntry, map[string][]byte, [][]byte) {
	t.Helper()
	unpacked := [][]byte{
		bootTar(t, []*tar.Header{{Name: "boot/vmlinuz-1", Typeflag: tar.TypeReg, Size: 3}}),
		bootTar(t, []*tar.Header{{Name: "boot/initrd.img-1", Typeflag: tar.TypeReg, Size: 7}}),
	}
	entry := dockerEntry{RepoTags: []string{tag}}
	objects := map[string][]byte{}
	config := v1.ConfigFile{OS: "linux", Architecture: architecture, RootFS: v1.RootFS{Type: "layers"}, Config: v1.Config{Env: []string{"FIXTURE=" + tag}}}
	for index, raw := range unpacked {
		config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", sha256.Sum256(raw))})
		var buffer bytes.Buffer
		switch compression {
		case "gzip":
			writer := gzip.NewWriter(&buffer)
			if _, err := writer.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			raw = buffer.Bytes()
		case "zstd":
			writer, err := zstd.NewWriter(&buffer)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			raw = buffer.Bytes()
		}
		object := fmt.Sprintf("layer-%d/layer.tar", index)
		objects[object] = raw
		entry.Layers = append(entry.Layers, object)
	}
	setDockerConfig(t, &entry, objects, encodeJSON(t, config))
	return entry, objects, unpacked
}

func setDockerConfig(t *testing.T, entry *dockerEntry, objects map[string][]byte, raw []byte) {
	t.Helper()
	delete(objects, entry.Config)
	entry.Config = fmt.Sprintf("%x.json", sha256.Sum256(raw))
	objects[entry.Config] = raw
}

func writeImageArchive(t *testing.T, objects map[string][]byte, compressed bool) string {
	t.Helper()
	var buffer bytes.Buffer
	var output io.Writer = &buffer
	zipper := gzip.NewWriter(&buffer)
	if compressed {
		output = zipper
	}
	writer := tar.NewWriter(output)
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		raw := objects[key]
		if err := writer.WriteHeader(&tar.Header{Name: key, Typeflag: tar.TypeReg, Size: int64(len(raw)), Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if compressed {
		if err := zipper.Close(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "image.bin")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readSourceLayers(ctx context.Context, source images.Source, platform images.Platform) (images.Manifest, [][]byte, error) {
	manifest, err := source.Resolve(ctx, platform)
	if err != nil {
		return images.Manifest{}, nil, err
	}
	var layers [][]byte
	for _, descriptor := range manifest.Layers {
		reader, err := source.OpenLayer(ctx, descriptor)
		if err != nil {
			return manifest, nil, err
		}
		raw, readErr := io.ReadAll(reader)
		if err := errors.Join(readErr, reader.Close()); err != nil {
			return manifest, nil, err
		}
		layers = append(layers, raw)
	}
	return manifest, layers, nil
}

func TestDockerSourcePreservesLayersAndIdentity(t *testing.T) {
	for _, compression := range []string{"raw", "gzip", "zstd"} {
		t.Run(compression, func(t *testing.T) {
			t.Parallel()
			entry, objects, expected := fixtureDockerEntry(t, "amd64", "example/demo:one", compression)
			objects["manifest.json"] = encodeJSON(t, []dockerEntry{entry})
			path := writeImageArchive(t, objects, false)
			source, cleanup, err := OpenLocal(t.Context(), path, t.TempDir(), LocalOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cleanup(); err != nil {
					t.Error(err)
				}
			})
			platform := images.Platform{OS: "linux", Architecture: "amd64"}
			manifest, layers, err := readSourceLayers(t.Context(), source, platform)
			if err != nil {
				t.Fatal(err)
			}
			if len(layers) != len(expected) {
				t.Fatalf("layer count = %d, want %d", len(layers), len(expected))
			}
			for index, raw := range layers {
				if !bytes.Equal(raw, expected[index]) {
					t.Fatalf("layer %d changed or reordered", index)
				}
			}
			// Repacking, renaming the input, and changing RepoTags must not
			// change the identity of the same config and ordered layers.
			entry.RepoTags = []string{"example/demo:alias"}
			objects["manifest.json"] = encodeJSON(t, []dockerEntry{entry})
			second, cleanSecond, err := OpenLocal(t.Context(), writeImageArchive(t, objects, true), t.TempDir(), LocalOptions{Format: FormatDocker})
			if err != nil {
				t.Fatal(err)
			}
			secondManifest, resolveErr := second.Resolve(t.Context(), platform)
			if err := errors.Join(resolveErr, cleanSecond()); err != nil {
				t.Fatal(err)
			}
			if secondManifest.Digest != manifest.Digest {
				t.Fatalf("repacked identity = %s, want %s", secondManifest.Digest, manifest.Digest)
			}
		})
	}
}

func TestDockerSourceSelectsTagAndPlatform(t *testing.T) {
	for _, test := range []struct {
		name         string
		tag          string
		architecture string
		wantError    bool
	}{
		{name: "ambiguous", architecture: "amd64", wantError: true},
		{name: "tag", tag: "example/demo:two", architecture: "amd64"},
		{name: "canonical tag", tag: "docker.io/example/demo:two", architecture: "amd64"},
		{name: "platform", architecture: "arm64"},
		{name: "missing tag", tag: "example/demo:missing", architecture: "amd64", wantError: true},
		{name: "wrong platform", tag: "example/demo:two", architecture: "arm64", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			objects := map[string][]byte{}
			var entries []dockerEntry
			for _, image := range []struct{ architecture, tag string }{{"amd64", "example/demo:one"}, {"amd64", "example/demo:two"}, {"arm64", "example/demo:arm"}} {
				entry, files, _ := fixtureDockerEntry(t, image.architecture, image.tag, "raw")
				entries = append(entries, entry)
				maps.Copy(objects, files)
			}
			objects["manifest.json"] = encodeJSON(t, entries)
			source, cleanup, err := OpenLocal(t.Context(), writeImageArchive(t, objects, false), t.TempDir(), LocalOptions{SourceTag: test.tag})
			if err != nil {
				t.Fatal(err)
			}
			_, _, readErr := readSourceLayers(t.Context(), source, images.Platform{OS: "linux", Architecture: test.architecture})
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if test.wantError {
				if code, _ := errdefs.CodeOf(readErr); code != errdefs.CodeInvalidArgument {
					t.Fatalf("selection error = %v", readErr)
				}
			} else if readErr != nil {
				t.Fatal(readErr)
			}
		})
	}
}

func TestDockerSourceRejectsCorruptionAndUnsafeReferences(t *testing.T) {
	for _, test := range []struct {
		name string
		code errdefs.Code
	}{
		{name: "config digest", code: errdefs.CodeDigestMismatch},
		{name: "layer diffID", code: errdefs.CodeDigestMismatch},
		{name: "config traversal", code: errdefs.CodeInvalidArgument},
		{name: "layer traversal", code: errdefs.CodeInvalidArgument},
		{name: "absolute layer", code: errdefs.CodeInvalidArgument},
		{name: "layer count", code: errdefs.CodeInvalidArgument},
		{name: "missing layer", code: errdefs.CodeArtifactUnavailable},
		{name: "layer limit", code: errdefs.CodeInvalidArgument},
		{name: "unpacked limit", code: errdefs.CodeInvalidArgument},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			entry, objects, _ := fixtureDockerEntry(t, "amd64", "example/demo:one", "raw")
			limits := images.DefaultLimits()
			switch test.name {
			case "config digest":
				objects[entry.Config] = append(objects[entry.Config], '\n')
			case "layer diffID":
				objects[entry.Layers[0]][512] ^= 1
			case "config traversal":
				entry.Config = "../" + entry.Config
			case "layer traversal":
				entry.Layers[0] = "../escape"
			case "absolute layer":
				entry.Layers[0] = "/etc/passwd"
			case "layer count":
				entry.Layers = entry.Layers[:1]
			case "missing layer":
				delete(objects, entry.Layers[0])
			case "layer limit":
				limits.LayerSize = 32
			case "unpacked limit":
				limits.UnpackedSize = 32
			}
			objects["manifest.json"] = encodeJSON(t, []dockerEntry{entry})
			staging := t.TempDir()
			source, cleanup, err := OpenLocal(t.Context(), writeImageArchive(t, objects, false), staging, LocalOptions{Limits: limits})
			if err == nil {
				_, _, err = readSourceLayers(t.Context(), source, images.Platform{OS: "linux", Architecture: "amd64"})
				if cleanupErr := cleanup(); cleanupErr != nil {
					t.Fatal(cleanupErr)
				}
			}
			if code, _ := errdefs.CodeOf(err); code != test.code {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
			files, err := os.ReadDir(staging)
			if err != nil || len(files) != 0 {
				t.Fatalf("staging leaked: %v, %v", files, err)
			}
		})
	}
}

func TestDockerSourceBlobPaths(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%v", corrupt), func(t *testing.T) {
			t.Parallel()
			entry, objects, expected := fixtureDockerEntry(t, "amd64", "example/demo:one", "gzip")
			config := objects[entry.Config]
			delete(objects, entry.Config)
			entry.Config = fmt.Sprintf("blobs/sha256/%x", sha256.Sum256(config))
			objects[entry.Config] = config
			for index, object := range entry.Layers {
				raw := objects[object]
				delete(objects, object)
				entry.Layers[index] = fmt.Sprintf("blobs/sha256/%x", sha256.Sum256(raw))
				objects[entry.Layers[index]] = raw
			}
			if corrupt {
				objects[entry.Layers[0]][10] ^= 1
			}
			objects["manifest.json"] = encodeJSON(t, []dockerEntry{entry})
			source, cleanup, err := OpenLocal(t.Context(), writeImageArchive(t, objects, false), t.TempDir(), LocalOptions{Format: FormatDocker})
			if err != nil {
				t.Fatal(err)
			}
			_, layers, readErr := readSourceLayers(t.Context(), source, images.Platform{OS: "linux", Architecture: "amd64"})
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if code, _ := errdefs.CodeOf(readErr); code != errdefs.CodeDigestMismatch {
					t.Fatalf("blob corruption error = %v", readErr)
				}
			} else if readErr != nil || len(layers) != len(expected) || !bytes.Equal(layers[0], expected[0]) || !bytes.Equal(layers[1], expected[1]) {
				t.Fatalf("blob layers changed: %v", readErr)
			}
		})
	}
}
