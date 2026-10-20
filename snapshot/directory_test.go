package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirectoryRoundTripRequiresCompleteFlatFileSet(t *testing.T) {
	source := t.TempDir()
	for name, data := range map[string]string{"cow.raw": "disk", "config.json": "{}", "state.json": "state"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exported := t.TempDir()
	record := archiveTestRecord()
	if err := WriteDirectory(t.Context(), source, exported, record); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	got, err := StageDirectory(t.Context(), exported, stage)
	if err != nil || got.ID != record.ID {
		t.Fatalf("stage directory = %+v, %v", got, err)
	}
	for name := range map[string]bool{"cow.raw": true, "config.json": true, "state.json": true} {
		if _, err := os.Stat(filepath.Join(stage, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(exported, "extra"), []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StageDirectory(t.Context(), exported, t.TempDir()); err == nil {
		t.Fatal("unexpected directory entry was accepted")
	}
	if err := os.Remove(filepath.Join(exported, "extra")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(exported, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := StageDirectory(t.Context(), exported, t.TempDir()); err == nil {
		t.Fatal("incomplete directory was accepted")
	}
}

func TestDirectoryMemorySharesCacheWhileWritableFilesStayPrivate(t *testing.T) {
	source := t.TempDir()
	for _, name := range []string{"cow.raw", "config.json", "state.json", "memory-range-0"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exported := t.TempDir()
	if err := WriteDirectory(t.Context(), source, exported, archiveTestRecord()); err != nil {
		t.Fatal(err)
	}
	first, second := t.TempDir(), t.TempDir()
	for _, stage := range []string{first, second} {
		if _, err := StageDirectory(t.Context(), exported, stage); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"cow.raw", "config.json", "state.json", "memory-range-0"} {
		original, err := os.Stat(filepath.Join(exported, name))
		if err != nil {
			t.Fatal(err)
		}
		staged, err := os.Stat(filepath.Join(first, name))
		if err != nil {
			t.Fatal(err)
		}
		if shared := os.SameFile(original, staged); shared != (name == "memory-range-0") {
			t.Fatalf("%s shares source inode = %v", name, shared)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "cow.raw"), []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{exported, second} {
		data, err := os.ReadFile(filepath.Join(directory, "cow.raw"))
		if err != nil || string(data) != "original" {
			t.Fatalf("sibling disk changed: %q, %v", data, err)
		}
	}
	if err := os.RemoveAll(exported); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(second, "memory-range-0"))
	if err != nil || string(data) != "original" {
		t.Fatalf("removing source or sibling invalidated memory: %q, %v", data, err)
	}
}

func TestDirectoryRejectsMemorySymlinksAndChangedSizes(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "memory-range-0"), []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	exported := t.TempDir()
	if err := WriteDirectory(t.Context(), source, exported, archiveTestRecord()); err != nil {
		t.Fatal(err)
	}
	memory := filepath.Join(exported, "memory-range-0")
	if err := os.WriteFile(memory, []byte("changed size"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StageDirectory(t.Context(), exported, t.TempDir()); err == nil {
		t.Fatal("accepted a memory file with a changed size")
	}
	if err := os.Remove(memory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "memory-range-0"), memory); err != nil {
		t.Fatal(err)
	}
	if _, err := StageDirectory(t.Context(), exported, t.TempDir()); err == nil {
		t.Fatal("accepted a symlink as native memory")
	}
}
