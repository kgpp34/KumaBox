package disk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStageAndFinalizeFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "source.raw")
	destination := filepath.Join(dir, "destination.raw")
	if err := os.WriteFile(source, []byte("snapshot payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	staged, err := StageFile(context.Background(), source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if staged.Strategy == "" || staged.SHA256 != "" {
		t.Fatalf("staged result = %+v", staged)
	}
	finalized, err := FinalizeStagedFile(context.Background(), destination, staged)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.SHA256 == "" || finalized.LogicalSizeBytes != int64(len("snapshot payload")) {
		t.Fatalf("finalized result = %+v", finalized)
	}
}

func TestProbeReflinkUsesRequestedDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	probe, err := ProbeReflink(dir)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Directory != dir {
		t.Fatalf("probe directory = %q, want %q", probe.Directory, dir)
	}
}

func TestProbeReflinkRejectsFile(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeReflink(file); err == nil {
		t.Fatal("expected file path to be rejected")
	}
}
