package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/storage"
)

func newImageTestExecutor(t *testing.T) (storage.Roots, func(...string) (string, error)) {
	t.Helper()
	base := t.TempDir()
	// This stand-in consumes tar input and writes deterministic bytes. Real EROFS is a Linux runbook check.
	binary := filepath.Join(base, "mkfs.erofs")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'mkfs.erofs 1.8.10\\n'; exit 0; fi\nfor output do :; done\n/bin/cat > \"$output\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", base+string(os.PathListSeparator)+os.Getenv("PATH"))
	roots := storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	execute := func(args ...string) (string, error) {
		command := NewCommand(func() storage.Roots { return roots })
		var out, stderr bytes.Buffer
		command.SetOut(&out)
		command.SetErr(&stderr)
		command.SetArgs(args)
		err := command.ExecuteContext(t.Context())
		return out.String(), err
	}
	return roots, execute
}

func TestImageCommandsFromLayoutAndArchive(t *testing.T) {
	roots, execute := newImageTestExecutor(t)
	base := filepath.Dir(roots.Data)
	if out, err := execute("ls", "--json"); err != nil || out != "[]\n" {
		t.Fatalf("empty list = %q, %v", out, err)
	}
	if _, err := execute("import", "tiny", "../../testdata/oci-layout", "--platform", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("verify", "tiny"); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, "fixture.bin")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(compressed)
	if err := filepath.Walk("../../testdata/oci-layout", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		relative, err := filepath.Rel("../../testdata/oci-layout", path)
		if err != nil {
			return err
		}
		header := &tar.Header{Name: relative, Typeflag: tar.TypeReg, Size: info.Size(), Mode: 0o600}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tarWriter.Write(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("import", "alias", archive, "--platform", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	out, err := execute("inspect", "tiny")
	if err != nil {
		t.Fatal(err)
	}
	var image imageOutput
	if err := json.Unmarshal([]byte(out), &image); err != nil {
		t.Fatal(err)
	}
	if len(image.Names) != 2 || len(image.Layers) != 1 {
		t.Fatalf("inspect = %s", out)
	}
	if _, err := execute("rm", "tiny", "alias"); err != nil {
		t.Fatal(err)
	}
	if out, err := execute("ls", "--json"); err != nil || out != "[]\n" {
		t.Fatalf("removed list = %q, %v", out, err)
	}
	paths, err := images.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(paths.StagingDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging entries = %v, %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(roots.Data, "images", "blobs")); !os.IsNotExist(err) {
		t.Fatalf("persistent OCI blobs exist: %v", err)
	}
}

func TestImageCommandsFromDockerArchive(t *testing.T) {
	roots, execute := newImageTestExecutor(t)
	base := filepath.Dir(roots.Data)
	fixture, err := layout.FromPath("../../testdata/oci-layout")
	if err != nil {
		t.Fatal(err)
	}
	index, err := fixture.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	image, err := fixture.Image(manifest.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag("example/demo:one")
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, "docker.bin")
	// Use the dependency's Docker archive writer as an independent producer.
	if err := tarball.WriteToFile(archive, tag, image); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"import", "docker-first", archive, "--platform", "linux/amd64"},
		{"verify", "docker-first"},
		{"import", "docker-alias", archive, "--platform", "linux/amd64", "--format", "docker", "--source-tag", "example/demo:one"},
		{"verify", "docker-alias"},
	} {
		if _, err := execute(args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	out, err := execute("inspect", "docker-first")
	if err != nil {
		t.Fatal(err)
	}
	var first imageOutput
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Names) != 2 || len(first.Layers) != 1 || first.Boot.KernelFile == "" || first.Boot.InitrdFile == "" {
		t.Fatalf("Docker inspect = %s", out)
	}
	if _, err := execute("import", "docker-first", archive, "--platform", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	repeated, err := execute("inspect", "docker-first")
	if err != nil || repeated != out {
		t.Fatalf("repeated import changed metadata: %s, %v", repeated, err)
	}
	if _, err := execute("import", "oci-reference", "../../testdata/oci-layout", "--platform", "linux/amd64", "--format", "oci"); err != nil {
		t.Fatal(err)
	}
	layers, err := os.ReadDir(filepath.Join(roots.Data, "images", "layers", "sha256"))
	if err != nil || len(layers) != 1 {
		t.Fatalf("Docker and OCI did not reuse the layer: %v, %v", layers, err)
	}
	// Two distinct configs sharing a layer still represent two source images.
	secondImage, err := mutate.Config(image, v1.Config{Env: []string{"VARIANT=two"}})
	if err != nil {
		t.Fatal(err)
	}
	secondTag, err := name.NewTag("example/demo:two")
	if err != nil {
		t.Fatal(err)
	}
	multi := filepath.Join(base, "multi.tar")
	if err := tarball.MultiWriteToFile(multi, map[name.Tag]v1.Image{tag: image, secondTag: secondImage}); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("import", "ambiguous", multi, "--platform", "linux/amd64"); err == nil {
		t.Fatal("ambiguous Docker archive was imported")
	}
	if _, err := execute("inspect", "ambiguous"); err == nil {
		t.Fatal("failed Docker import became visible")
	}
	if _, err := execute("import", "selected", multi, "--platform", "linux/amd64", "--source-tag", "example/demo:two"); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("verify", "selected"); err != nil {
		t.Fatal(err)
	}
	staging, err := os.ReadDir(filepath.Join(roots.Data, "staging", "imports"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("Docker import left staging: %v, %v", staging, err)
	}
	if _, err := execute("rm", "docker-first", "docker-alias", "oci-reference", "selected"); err != nil {
		t.Fatal(err)
	}
	if out, err := execute("ls", "--json"); err != nil || out != "[]\n" {
		t.Fatalf("removed Docker list = %q, %v", out, err)
	}
}

func TestImportRejectsUnknownFormatBeforeOpeningStore(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	command := NewCommand(func() storage.Roots { return roots })
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"import", "demo", "missing.tar", "--format", "tar"})
	if err := command.ExecuteContext(t.Context()); err == nil {
		t.Fatal("unknown format was accepted")
	} else if code, _ := errdefs.CodeOf(err); code != errdefs.CodeInvalidArgument {
		t.Fatalf("format error = %v", err)
	}
	if _, err := os.Stat(roots.Data); !os.IsNotExist(err) {
		t.Fatalf("invalid format created a store: %v", err)
	}
}
