package cloudhypervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kumabox/kumabox/types"
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
			if got, err := driver.restoreMemoryMode(t.Context(), path, types.MemoryRestoreAuto); err != nil || got != test.want {
				t.Fatalf("restoreMemoryMode() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestExplicitMemoryRestoreModesRequireCompatibleMonitorAndBacking(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		mode    types.MemoryRestoreMode
		shared  bool
		want    string
		wantErr bool
	}{
		{"copy on old monitor", "v52.0", types.MemoryRestoreCopy, false, "", false},
		{"v53 auto prepays memory", "v53.0", types.MemoryRestoreAuto, false, "", false},
		{"v54 auto shares pages", "v54.0-dev", types.MemoryRestoreAuto, false, memoryModeCopyOnWrite, false},
		{"lazy on v53", "v53.0", types.MemoryRestoreOnDemand, false, memoryModeOnDemand, false},
		{"mmap on v53", "v53.0", types.MemoryRestoreMmap, false, "", true},
		{"mmap on v54", "v54.0-dev", types.MemoryRestoreMmap, false, memoryModeCopyOnWrite, false},
		{"lazy on unknown monitor", "unknown", types.MemoryRestoreOnDemand, false, "", true},
		{"auto on unknown monitor", "unknown", types.MemoryRestoreAuto, false, "", false},
		{"shared auto", "v54.0", types.MemoryRestoreAuto, true, "", false},
		{"shared explicit mmap", "v54.0", types.MemoryRestoreMmap, true, "", true},
		{"shared explicit lazy", "v54.0", types.MemoryRestoreOnDemand, true, "", true},
		{"invalid mode", "v54.0", "invalid", false, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			binary := filepath.Join(directory, "monitor")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'cloud-hypervisor "+test.version+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "config.json")
			config := `{"memory":{"size":1073741824,"shared":false}}`
			if test.shared {
				config = `{"memory":{"size":1073741824,"shared":true}}`
			}
			if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			driver := &Driver{binary: binary}
			got, err := driver.restoreMemoryMode(t.Context(), path, test.mode)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("restoreMemoryMode() = %q, %v; want %q, error=%v", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestMemorySupportProbeCachesSuccessButRetriesFailureAndCancellation(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "monitor")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := &Driver{binary: binary}
	if _, err := driver.supportedMemoryMode(t.Context()); err == nil {
		t.Fatal("failed monitor probe was accepted")
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf x >> \"$0.calls\"\necho 'cloud-hypervisor v54.0'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := driver.supportedMemoryMode(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled capability probe = %v", err)
	}
	var callers sync.WaitGroup
	for range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			mode, err := driver.supportedMemoryMode(t.Context())
			if err != nil || mode != memoryModeCopyOnWrite {
				t.Errorf("capability retry = %q, %v", mode, err)
			}
		}()
	}
	callers.Wait()
	data, err := os.ReadFile(binary + ".calls")
	if err != nil || string(data) != "x" {
		t.Fatalf("successful probe executed repeatedly: %q, %v", data, err)
	}
}
