//go:build linux

package core

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kumabox/kumabox/snapshot"
	"github.com/kumabox/kumabox/types"
)

func TestSnapshotArchiveImportsIntoAnotherRoot(t *testing.T) {
	source, _, _ := newTestSnapshotService(t)
	sourceImage := source.lifecycle.dependencies.images.(fakeGuard)
	sourceImage.image.RegistryReference = "registry.example.test/team/guest:v1"
	source.lifecycle.dependencies.images = sourceImage
	source.images = sourceImage
	record, err := source.Save(t.Context(), SaveSnapshotRequest{SandboxReference: "box", Name: "warm"})
	if err != nil {
		t.Fatal(err)
	}
	cow, err := source.paths.COW(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(cow, record.Config.Storage); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cow)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks*512 >= info.Size()/2 {
		t.Skip("test filesystem does not report sparse allocation")
	}
	var archive bytes.Buffer
	if _, err := source.Export(t.Context(), "warm", &archive, false); err != nil {
		t.Fatal(err)
	}
	if archive.Len() > 1<<20 {
		t.Fatalf("archive expanded the sparse COW to %d bytes", archive.Len())
	}
	exportedDir := t.TempDir()
	if _, err := source.ExportDirectory(t.Context(), "warm", exportedDir); err != nil {
		t.Fatal(err)
	}
	stagedDir := t.TempDir()
	stagedRecord, err := snapshot.StageDirectory(t.Context(), exportedDir, stagedDir)
	if err != nil || stagedRecord.ImageDigest != record.ImageDigest {
		t.Fatalf("direct directory = %+v, %v", stagedRecord, err)
	}
	if _, err := os.Stat(filepath.Join(stagedDir, "cow.raw")); err != nil {
		t.Fatal(err)
	}
	restoreTarget, _, restoreSteps := newTestSnapshotService(t)
	if _, err := restoreTarget.RestoreWithOptions(t.Context(), "box", "", RestoreOptions{SourceDirectory: exportedDir}); err != nil {
		t.Fatalf("restore from directory: %v", err)
	}
	foundClone := false
	for _, step := range *restoreSteps {
		if step == "clone" {
			foundClone = true
		}
	}
	if !foundClone {
		t.Fatalf("directory restore did not rebind native state: %v", *restoreSteps)
	}
	remaining, err := os.ReadDir(restoreTarget.paths.StagingDir())
	if err != nil || len(remaining) != 0 {
		t.Fatalf("directory restore left staging files: %v, %v", remaining, err)
	}
	payload := bytes.Clone(archive.Bytes())
	target, _, steps := newTestSnapshotService(t)
	guard := target.lifecycle.dependencies.images.(fakeGuard)
	guard.afterUse = errors.New("image is not available at target root")
	target.lifecycle.dependencies.images = guard
	target.images = guard
	beforeImport := len(*steps)
	imported, err := target.Import(t.Context(), bytes.NewReader(payload), "transferred", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range (*steps)[beforeImport:] {
		if step == "verify" {
			t.Fatal("snapshot import required the target image")
		}
	}
	if imported.Name != "transferred" || imported.Config.Storage != types.DefaultSandboxStorage || imported.ImageDigest != record.ImageDigest || imported.RegistryReference != sourceImage.image.RegistryReference {
		t.Fatalf("imported snapshot = %+v", imported)
	}
	targetDir, err := target.paths.Dir(imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "cow.raw")); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Inspect(t.Context(), "transferred"); err != nil {
		t.Fatal(err)
	}
	failed, _, _ := newTestSnapshotService(t)
	if _, err := failed.Save(t.Context(), SaveSnapshotRequest{SandboxReference: "box", Name: "existing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := failed.Import(t.Context(), bytes.NewReader(payload), "rolled-back", ""); err == nil {
		t.Fatal("import reused an existing snapshot ID")
	}
	listed, err := failed.List(t.Context())
	if err != nil || len(listed) != 1 || listed[0].Name != "existing" {
		t.Fatalf("failed import retained metadata: %+v, %v", listed, err)
	}
	entries, err := os.ReadDir(failed.paths.StagingDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed import retained stage: %v, %v", entries, err)
	}
}
