package storage

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
