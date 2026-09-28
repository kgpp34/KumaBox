//go:build linux

package core

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kumabox/kumabox/types"
)

func TestSnapshotArchiveImportsIntoAnotherRoot(t *testing.T) {
	source, _, _ := newTestSnapshotService(t)
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
	payload := bytes.Clone(archive.Bytes())
	target, _, _ := newTestSnapshotService(t)
	imported, err := target.Import(t.Context(), bytes.NewReader(payload), "transferred", "")
	if err != nil {
		t.Fatal(err)
	}
	if imported.Name != "transferred" || imported.Config.Storage != types.DefaultSandboxStorage || imported.ImageDigest != record.ImageDigest {
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
	guard := failed.lifecycle.dependencies.images.(fakeGuard)
	guard.afterUse = errors.New("image availability changed")
	failed.lifecycle.dependencies.images = guard
	if _, err := failed.Import(t.Context(), bytes.NewReader(payload), "rolled-back", ""); !errors.Is(err, guard.afterUse) {
		t.Fatalf("import after image guard failure = %v", err)
	}
	listed, err := failed.List(t.Context())
	if err != nil || len(listed) != 0 {
		t.Fatalf("failed import retained metadata: %+v, %v", listed, err)
	}
	entries, err := os.ReadDir(failed.paths.StagingDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed import retained stage: %v, %v", entries, err)
	}
}
