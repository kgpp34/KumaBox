package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
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

type resumeScope struct {
	cleanupScope
	apply func(context.Context, types.SandboxID, uint32, types.CPUPolicy) error
}

func (s *resumeScope) ApplyLimit(ctx context.Context, id types.SandboxID, cpus uint32, policy types.CPUPolicy) error {
	return s.apply(ctx, id, cpus, policy)
}

func TestRestoreNeverResumesBeforeRuntimeLimit(t *testing.T) {
	for _, test := range []struct {
		name string
		fail string
		want []string
	}{
		{"success", "", []string{"restore", "devices", "limit", "resume"}},
		{"memory fails", "restore", []string{"restore"}},
		{"device rebind fails", "devices", []string{"restore", "devices"}},
		{"limit fails", "limit", []string{"restore", "devices", "limit"}},
		{"canceled before limit", "cancel", []string{"restore", "devices"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "kb-restore-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(directory) //nolint:errcheck // test-owned temporary directory
			socket := filepath.Join(directory, "api.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan string, 8)
			server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v1/vm.restore":
					events <- "restore"
					if test.fail == "restore" {
						writer.WriteHeader(http.StatusInternalServerError)
						return
					}
				case "/api/v1/vm.resume":
					events <- "resume"
				default:
					t.Errorf("unexpected API request %s", request.URL.Path)
				}
				writer.WriteHeader(http.StatusNoContent)
			})}
			go func() { _ = server.Serve(listener) }()
			defer server.Close() //nolint:errcheck // test cleanup
			failure := errors.New("injected failure")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			plan := vmm.RestorePlan{SandboxID: "123e4567-e89b-42d3-a456-426614174000", CPUs: 2, CPUPolicy: types.CPUPolicy{Weight: 37, CPUSet: "2-3"}, SnapshotDir: "/snapshot"}
			scopes := &resumeScope{apply: func(ctx context.Context, id types.SandboxID, cpus uint32, policy types.CPUPolicy) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if id != plan.SandboxID || cpus != plan.CPUs || policy != plan.CPUPolicy {
					t.Errorf("CPU policy applied to wrong target: %s, %d", id, cpus)
				}
				events <- "limit"
				if test.fail == "limit" {
					return failure
				}
				return nil
			}}
			driver := &Driver{scopes: scopes, startupTimeout: time.Second}
			err = driver.loadAndResume(ctx, plan, socket, memoryModeOnDemand, func(context.Context, string) error {
				events <- "devices"
				if test.fail == "cancel" {
					cancel()
				}
				if test.fail == "devices" {
					return failure
				}
				return nil
			})
			if (err != nil) != (test.fail != "") {
				t.Fatalf("loadAndResume() error = %v", err)
			}
			var got []string
			for len(events) > 0 {
				got = append(got, <-events)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("restore sequence = %v, want %v", got, test.want)
			}
		})
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
	if err := (*Driver)(nil).ValidateRestore(t.Context(), directory, types.MemoryRestoreCopy); err != nil {
		t.Fatalf("ValidateRestore() = %v", err)
	}
	if err := os.Remove(filepath.Join(directory, "memory-range-0")); err != nil {
		t.Fatal(err)
	}
	if err := (*Driver)(nil).ValidateRestore(t.Context(), directory, types.MemoryRestoreCopy); err == nil {
		t.Fatal("ValidateRestore accepted native state without memory")
	}
}
