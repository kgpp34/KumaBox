package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func TestWriteNativeManifestRejectsIncompletePayload(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	build, err := store.Reserve(context.Background(), "incomplete")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = build.Abort() })
	nativeDir := filepath.Join(build.Record().StagingDir, "native")
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = WriteNativeManifest(context.Background(), build, &vmstore.VMRecord{ID: "kb", Name: "vm"}, nil, backend.NativeHost{}, "crash")
	if err == nil {
		t.Fatal("expected incomplete native payload error")
	}
}
