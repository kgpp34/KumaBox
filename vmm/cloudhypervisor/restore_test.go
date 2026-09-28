package cloudhypervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRestorePayloadIncludesSelectedMemoryMode(t *testing.T) {
	for _, mode := range []string{"", memoryModeOnDemand, memoryModeCopyOnWrite} {
		payload, err := restorePayload("/data/native", mode)
		if err != nil {
			t.Fatal(err)
		}
		var request map[string]string
		if err := json.Unmarshal(payload, &request); err != nil {
			t.Fatal(err)
		}
		if request["source_url"] != "file:///data/native" || request["memory_restore_mode"] != mode {
			t.Fatalf("restore payload = %s", payload)
		}
		if mode == "" {
			if _, exists := request["memory_restore_mode"]; exists {
				t.Fatalf("default restore should omit memory mode: %s", payload)
			}
		}
	}
}

func TestValidateRestoreRequiresCompleteNativeSnapshot(t *testing.T) {
	directory := t.TempDir()
	for name, content := range map[string]string{
		"config.json":    `{"cpus":{"boot_vcpus":2}}`,
		"state.json":     `{"version":1}`,
		"memory-range-0": "memory",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := (*Driver)(nil).ValidateRestore(t.Context(), directory); err != nil {
		t.Fatalf("ValidateRestore() = %v", err)
	}
	if err := os.Remove(filepath.Join(directory, "memory-range-0")); err != nil {
		t.Fatal(err)
	}
	if err := (*Driver)(nil).ValidateRestore(t.Context(), directory); err == nil {
		t.Fatal("ValidateRestore accepted native state without memory")
	}
}
