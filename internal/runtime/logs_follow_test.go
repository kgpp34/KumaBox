package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/vm"
)

func TestFollowedLogReadsTailAppendTruncateAndReplacement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "vmm.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := followedLog{name: "vmm.log", path: path}
	content, changed, err := log.read(true, 2)
	if err != nil || !changed || content != "two\nthree\n" {
		t.Fatalf("initial read content=%q changed=%t err=%v", content, changed, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("four\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	content, changed, err = log.read(false, 0)
	if err != nil || !changed || content != "four\n" {
		t.Fatalf("append read content=%q changed=%t err=%v", content, changed, err)
	}
	if err := os.WriteFile(path, []byte("new-boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, changed, err = log.read(false, 0)
	if err != nil || !changed || content != "new-boot\n" {
		t.Fatalf("truncate read content=%q changed=%t err=%v", content, changed, err)
	}
	replacement := filepath.Join(dir, "replacement.log")
	if err := os.WriteFile(replacement, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	content, changed, err = log.read(false, 0)
	if err != nil || !changed || content != "replacement\n" {
		t.Fatalf("replacement read content=%q changed=%t err=%v", content, changed, err)
	}
}

func TestFollowLogsVMWaitsForFileAndStopsWithContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := vm.New(filepath.Join(dir, "data"))
	rec, err := store.Create(vm.CreateRequest{
		Name: "follow", RootDisk: "root.raw", Kernel: "vmlinuz", Initrd: "initrd",
		RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := NewWithBackend(store, backendFake{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	chunks := make(chan VMLogChunk, 1)
	go func() {
		done <- rt.FollowLogsVM(ctx, rec.ID, LogOptions{Source: LogSourceStderr, Tail: 1}, 10*time.Millisecond, func(chunk VMLogChunk) error {
			chunks <- chunk
			return nil
		})
	}()
	path := filepath.Join(rec.LogDir, "cloud-hypervisor.stderr.log")
	if err := os.MkdirAll(rec.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(rec.LogDir, ".delayed.log")
	if err := os.WriteFile(temporary, []byte("old-line\nlate-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
	select {
	case chunk := <-chunks:
		if chunk.Content != "late-line\n" || chunk.Name != "cloud-hypervisor.stderr.log" {
			t.Fatalf("chunk = %+v", chunk)
		}
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not observe a delayed log file")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFollowLogsVMRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	rt := &Runtime{}
	if err := rt.FollowLogsVM(t.Context(), "vm", LogOptions{}, 0, func(VMLogChunk) error { return nil }); err == nil {
		t.Fatal("expected non-positive interval error")
	}
	if err := rt.FollowLogsVM(t.Context(), "vm", LogOptions{}, time.Second, nil); err == nil {
		t.Fatal("expected nil emitter error")
	}
	if err := rt.FollowLogsVM(t.Context(), "vm", LogOptions{Source: "invalid"}, time.Second, func(VMLogChunk) error { return nil }); err == nil {
		t.Fatal("expected invalid source error")
	}
}
