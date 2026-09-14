package source

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
)

func TestParseFormat(t *testing.T) {
	for _, test := range []struct {
		value string
		want  Format
	}{
		{value: "", want: FormatAuto},
		{value: "auto", want: FormatAuto},
		{value: "docker", want: FormatDocker},
		{value: "oci", want: FormatOCI},
		{value: "tar"},
		{value: "docker-archive"},
	} {
		t.Run("format="+test.value, func(t *testing.T) {
			got, err := ParseFormat(test.value)
			if test.want == "" {
				if code, _ := errdefs.CodeOf(err); code != errdefs.CodeInvalidArgument {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil || got != test.want {
				t.Fatalf("format = %q, %v, want %q", got, err, test.want)
			}
		})
	}
}

func fixtureOCIObjects(t *testing.T) map[string][]byte {
	t.Helper()
	objects := map[string][]byte{}
	root := "../../testdata/oci-layout"
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		object, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		objects[object] = raw
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return objects
}

func TestOpenLocalFormats(t *testing.T) {
	for _, test := range []struct {
		name       string
		format     Format
		docker     bool
		directory  bool
		compressed bool
		wantError  bool
	}{
		{name: "OCI directory", directory: true},
		{name: "explicit OCI directory", directory: true, format: FormatOCI},
		{name: "OCI tar"},
		{name: "OCI gzip", compressed: true},
		{name: "explicit OCI archive", format: FormatOCI},
		{name: "Docker tar", docker: true},
		{name: "Docker gzip", docker: true, compressed: true},
		{name: "explicit Docker", docker: true, format: FormatDocker},
		{name: "Docker forced as OCI", docker: true, format: FormatOCI, wantError: true},
		{name: "OCI forced as Docker", format: FormatDocker, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			objects := fixtureOCIObjects(t)
			if test.docker {
				entry, files, _ := fixtureDockerEntry(t, "amd64", "example/demo:one", "raw")
				objects = files
				objects["manifest.json"] = encodeJSON(t, []dockerEntry{entry})
			}
			path := "../../testdata/oci-layout"
			if !test.directory {
				path = writeImageArchive(t, objects, test.compressed)
			}
			staging := t.TempDir()
			source, cleanup, err := OpenLocal(t.Context(), path, staging, LocalOptions{Format: test.format})
			if err == nil {
				_, _, err = readSourceLayers(t.Context(), source, images.Platform{OS: "linux", Architecture: "amd64"})
				if cleanupErr := cleanup(); cleanupErr != nil {
					t.Fatal(cleanupErr)
				}
			}
			if test.wantError {
				if err == nil {
					t.Fatal("accepted mismatched format")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			files, err := os.ReadDir(staging)
			if err != nil || len(files) != 0 {
				t.Fatalf("staging leaked: %v, %v", files, err)
			}
		})
	}
}

func TestOpenLocalPrefersOCIWithoutFallback(t *testing.T) {
	objects := fixtureOCIObjects(t)
	entry, dockerObjects, _ := fixtureDockerEntry(t, "amd64", "example/demo:one", "raw")
	maps.Copy(objects, dockerObjects)
	objects["manifest.json"] = encodeJSON(t, []dockerEntry{entry})
	platform := images.Platform{OS: "linux", Architecture: "amd64"}
	fixture, err := NewLayout("../../testdata/oci-layout")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := fixture.Resolve(t.Context(), platform)
	if err != nil {
		t.Fatal(err)
	}
	source, cleanup, err := OpenLocal(t.Context(), writeImageArchive(t, objects, false), t.TempDir(), LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manifest, resolveErr := source.Resolve(t.Context(), platform)
	if err := errors.Join(resolveErr, cleanup()); err != nil {
		t.Fatal(err)
	}
	if manifest.Digest != expected.Digest {
		t.Fatal("auto detection did not prefer OCI metadata")
	}
	objects["oci-layout"] = []byte("broken")
	source, cleanup, err = OpenLocal(t.Context(), writeImageArchive(t, objects, false), t.TempDir(), LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, resolveErr = source.Resolve(t.Context(), platform)
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if resolveErr == nil {
		t.Fatal("corrupt OCI metadata silently fell back to Docker")
	}
}

func TestOpenLocalRejectsUnknownAndBoundsArchives(t *testing.T) {
	for _, test := range []struct {
		name    string
		options LocalOptions
	}{
		{name: "docker export is not docker save"},
		{name: "archive size", options: LocalOptions{Limits: images.Limits{LayerSize: 1024, UnpackedSize: 1024, BootSize: 1024, ArchiveSize: 64}}},
		{name: "invalid limits", options: LocalOptions{Limits: images.Limits{ArchiveSize: 64}}},
		{name: "invalid format", options: LocalOptions{Format: "tar"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeImageArchive(t, map[string][]byte{"etc/os-release": []byte("fixture")}, true)
			staging := t.TempDir()
			_, _, err := OpenLocal(t.Context(), path, staging, test.options)
			if code, _ := errdefs.CodeOf(err); code != errdefs.CodeInvalidArgument {
				t.Fatalf("error = %v", err)
			}
			files, err := os.ReadDir(staging)
			if err != nil || len(files) != 0 {
				t.Fatalf("staging leaked: %v, %v", files, err)
			}
		})
	}
}

func TestOpenLocalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := OpenLocal(ctx, "missing", t.TempDir(), LocalOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("open cancellation = %v", err)
	}
	entry, objects, _ := fixtureDockerEntry(t, "amd64", "example/demo:one", "raw")
	objects["manifest.json"] = encodeJSON(t, []dockerEntry{entry})
	source, cleanup, err := OpenLocal(t.Context(), writeImageArchive(t, objects, false), t.TempDir(), LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, resolveErr := source.Resolve(ctx, images.Platform{OS: "linux", Architecture: "amd64"})
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(resolveErr, context.Canceled) {
		t.Fatalf("resolve cancellation = %v", resolveErr)
	}
}
