package cloudhypervisor

import (
	"os"
	"path/filepath"
	"testing"
)

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
