package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/vmm"
)

func TestReplaceWritableSetRestoresAllDisksOnPartialFailure(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, ".restore-backup")
	files := []vmm.SnapshotFile{
		{Source: filepath.Join(root, "staged-cow"), Destination: filepath.Join(root, "cow.raw")},
		{Source: filepath.Join(root, "staged-db"), Destination: filepath.Join(root, "data-db.raw")},
	}
	for _, file := range files {
		if err := os.WriteFile(file.Source, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.Destination, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(source, destination string) error {
		if source == files[1].Source {
			return errors.New("injected data disk publication failure")
		}
		return storage.Publish(source, destination)
	}
	if err := replaceWritableSet(files, backupDir, publish); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("replacement error = %v", err)
	}
	for _, file := range files {
		content, err := os.ReadFile(file.Destination)
		if err != nil || string(content) != "old" {
			t.Fatalf("disk %s after rollback = %q, %v", file.Destination, content, err)
		}
	}
	if _, err := os.Stat(backupDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup directory remains after rollback: %v", err)
	}
}

func TestReplaceWritableSetPublishesCompleteSet(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, ".restore-backup")
	files := []vmm.SnapshotFile{
		{Source: filepath.Join(root, "staged-cow"), Destination: filepath.Join(root, "cow.raw")},
		{Source: filepath.Join(root, "staged-db"), Destination: filepath.Join(root, "data-db.raw")},
	}
	for _, file := range files {
		if err := os.WriteFile(file.Source, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.Destination, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := replaceWritableSet(files, backupDir, storage.Publish); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		content, err := os.ReadFile(file.Destination)
		if err != nil || string(content) != "new" {
			t.Fatalf("disk %s after restore = %q, %v", file.Destination, content, err)
		}
	}
}

func TestRecoverWritableSetRollsBackInterruptedReplacement(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, ".restore-backup")
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cow := filepath.Join(root, "cow.raw")
	data := filepath.Join(root, "data-db.raw")
	staged := filepath.Join(root, "staged-cow")
	for path, content := range map[string]string{cow: "old-cow", data: "old-data", staged: "new-cow"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Publish(cow, filepath.Join(backupDir, "cow.raw")); err != nil {
		t.Fatal(err)
	}
	if err := storage.Publish(staged, cow); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, restoreCommitMarker+".tmp"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverWritableSet(backupDir, []string{cow, data}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{cow: "old-cow", data: "old-data"} {
		content, err := os.ReadFile(path)
		if err != nil || string(content) != want {
			t.Fatalf("recovered %s = %q, %v", path, content, err)
		}
	}
}

func TestRecoverWritableSetKeepsCommittedReplacement(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, ".restore-backup")
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cow := filepath.Join(root, "cow.raw")
	if err := os.WriteFile(cow, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "cow.raw"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, restoreCommitMarker), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverWritableSet(backupDir, []string{cow}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(cow)
	if err != nil || string(content) != "new" {
		t.Fatalf("committed COW = %q, %v", content, err)
	}
}
