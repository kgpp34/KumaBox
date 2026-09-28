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
