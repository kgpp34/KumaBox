package cloudhypervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryModeForVersion(t *testing.T) {
	for _, test := range []struct {
		version string
		want    string
	}{
		{"cloud-hypervisor v52.0", ""},
		{"cloud-hypervisor v53.0", memoryModeOnDemand},
		{"cloud-hypervisor v54.0-dev", memoryModeCopyOnWrite},
		{"unknown", ""},
	} {
		if got := memoryModeForVersion(test.version); got != test.want {
			t.Errorf("memoryModeForVersion(%q) = %q, want %q", test.version, got, test.want)
		}
	}
}

func TestCloneMemoryModeRejectsUnsupportedMemoryBacking(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "monitor")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'cloud-hypervisor v54.0'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := &Driver{binary: binary}
	for _, test := range []struct {
		name   string
		config string
		want   string
	}{
		{"ordinary", `{"memory":{"size":1073741824}}`, memoryModeCopyOnWrite},
		{"shared", `{"memory":{"size":1073741824,"shared":true}}`, ""},
		{"hugepages", `{"memory":{"size":1073741824,"hugepages":true}}`, ""},
		{"shared zone", `{"memory":{"size":1073741824,"zones":[{"shared":true}]}}`, ""},
		{"missing size", `{"memory":{}}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, "config.json")
			if err := os.WriteFile(path, []byte(test.config), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := driver.cloneMemoryMode(t.Context(), path); got != test.want {
				t.Fatalf("cloneMemoryMode() = %q, want %q", got, test.want)
			}
		})
	}
}
