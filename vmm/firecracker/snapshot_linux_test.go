//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/vmm"
)

// A real process identity and Unix API exercise pause/capture/resume failures
// without KVM, root privileges or access to the machine's actual VMM cgroups.
func TestSnapshotResumesAfterCaptureFailureAndCancellation(t *testing.T) {
	for _, failure := range []string{"none", "pause response", "capture", "disk copy", "cancellation", "persist failure", "hibernate", "termination cancellation"} {
		t.Run(failure, func(t *testing.T) {
			f := testSnapshot(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dir, err := os.MkdirTemp("/tmp", "kb-fc-cap-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			socket := filepath.Join(dir, "api.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var steps []string
			server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					_ = json.NewEncoder(w).Encode(map[string]string{"state": "Running"})
				case "/vm/config":
					_ = json.NewEncoder(w).Encode(f.meta.Config)
				case "/vm":
					var state struct {
						State string `json:"state"`
					}
					if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					mu.Lock()
					steps = append(steps, state.State)
					mu.Unlock()
					if failure == "pause response" && state.State == "Paused" {
						http.Error(w, "pause may have committed", 500)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case "/snapshot/create":
					mu.Lock()
					steps = append(steps, "capture")
					mu.Unlock()
					if failure == "capture" {
						http.Error(w, "capture failed", 500)
						return
					}
					if failure == "cancellation" {
						cancel()
						<-r.Context().Done()
						return
					}
					var request snapshotCreate
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if request.Type != "Full" {
						t.Errorf("snapshot type = %q", request.Type)
					}
					if err := os.WriteFile(request.State, []byte("native state"), 0o600); err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					memory, err := os.Create(request.Memory)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					if err := memory.Truncate(f.meta.Config.Machine.MemoryMiB << 20); err != nil {
						t.Error(err)
					}
					if err := memory.Close(); err != nil {
						t.Error(err)
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			input, keepOpen, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()    //nolint:errcheck
			defer keepOpen.Close() //nolint:errcheck
			command := exec.Command("/bin/sh", "-c", "read line", "--api-sock", socket)
			command.Stdin = input
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			reaped := make(chan struct{})
			go func() { _ = command.Wait(); close(reaped) }()
			defer func() { _ = command.Process.Kill(); <-reaped }()
			process, err := vmm.CaptureProcess(command.Process.Pid, f.meta.SandboxID, 1, "sh", socket)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.driver.paths.Prepare(f.meta.SandboxID); err != nil {
				t.Fatal(err)
			}
			if err := f.driver.paths.WriteProcess(process); err != nil {
				t.Fatal(err)
			}
			f.driver.options = Options{Binary: "sh", StartupTimeout: time.Second, StopGrace: time.Second, AbortGrace: time.Second}
			plan := vmm.SnapshotPlan{Process: process, Destination: t.TempDir()}
			plan.WritableFiles = []vmm.SnapshotFile{{Source: f.meta.Writable[0].Source, Destination: filepath.Join(plan.Destination, "cow.raw")}}
			if failure == "disk copy" {
				if err := os.Remove(plan.WritableFiles[0].Source); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "persist failure" || failure == "hibernate" || failure == "termination cancellation" {
				err = f.driver.Hibernate(ctx, plan, func() error {
					mu.Lock()
					steps = append(steps, "persist")
					mu.Unlock()
					if failure == "persist failure" {
						return errors.New("publish failed")
					}
					if failure == "termination cancellation" {
						cancel()
					}
					return nil
				})
			} else {
				err = f.driver.Snapshot(ctx, plan)
			}
			if (err == nil) != (failure == "none" || failure == "hibernate") {
				t.Fatalf("snapshot result = %v", err)
			}
			if failure == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			mu.Lock()
			got := slices.Clone(steps)
			mu.Unlock()
			want := []string{"Paused", "capture", "Resumed"}
			if failure == "persist failure" {
				want = []string{"Paused", "capture", "persist", "Resumed"}
			}
			if failure == "hibernate" || failure == "termination cancellation" {
				want = []string{"Paused", "capture", "persist"}
			}
			if failure == "pause response" {
				want = []string{"Paused", "Resumed"}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("state transitions = %v, want %v", got, want)
			}
			if failure == "hibernate" {
				if alive, err := vmm.VerifyProcess(process); err != nil || alive {
					t.Fatalf("hibernated process still alive: %v %v", alive, err)
				}
			}
			if failure == "termination cancellation" {
				if alive, err := vmm.VerifyProcess(process); err != nil || !alive {
					t.Fatalf("unexpected canceled termination result: %v %v", alive, err)
				}
			}
			if failure == "none" {
				if err := f.driver.ValidateSnapshot(t.Context(), plan.Destination); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(plan.Destination, "cow.raw"))
				if err != nil || string(data) != "captured" {
					t.Fatalf("captured disk = %q %v", data, err)
				}
			}
		})
	}
}
