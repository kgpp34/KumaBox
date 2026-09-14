package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/storage"
)

func TestImageCommandsFromLayoutAndArchive(t *testing.T) {
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
