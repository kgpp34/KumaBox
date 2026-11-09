//go:build linux

package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

func captureTestDriver(t *testing.T, handler http.Handler) (*Driver, vmm.SnapshotPlan) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "kb-capture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	paths, err := vmm.NewPaths(storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")})
	if err != nil {
		t.Fatal(err)
	}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	if err := paths.Prepare(id); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(base, "api.sock")
	command, _ := startProcessHelper(t, false, socket)
	process, err := captureProcess(command.Process.Pid, id, 3, filepath.Base(os.Args[0]), socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.WriteProcess(process); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	source, destination := filepath.Join(base, "cow.raw"), filepath.Join(base, "capture")
	if err := os.WriteFile(source, []byte("guest data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Driver{paths: paths, scopes: &cleanupScope{}, stopGrace: time.Second, startupTimeout: time.Second}, vmm.SnapshotPlan{
		Process: process, Destination: destination,
		WritableFiles: []vmm.SnapshotFile{{Source: source, Destination: filepath.Join(destination, "cow.raw")}},
	}
}

func TestSnapshotCancellationWaitsForDumpAndResumes(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finish)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var state atomic.Value
	state.Store("Running")
	var dumped atomic.Bool
	driver, plan := captureTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vm.info":
			_, _ = fmt.Fprintf(w, `{"state":%q}`, state.Load())
		case "/api/v1/vm.pause":
			state.Store("Paused")
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/vm.snapshot":
			cancel()
			close(started)
			<-release
			dumped.Store(true)
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/vm.resume":
			if !dumped.Load() {
				t.Error("resumed before the dump completed")
			}
			state.Store("Running")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected action %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	result := make(chan error, 1)
	go func() { result <- driver.Snapshot(ctx, plan) }()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("capture never started: %v", err)
	}
	select {
	case err := <-result:
		t.Fatalf("capture returned while the dump was writing: %v", err)
	default:
	}
	finish()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot = %v", err)
	}
	if state.Load() != "Running" {
		t.Fatal("guest remained paused")
	}
	if _, err := driver.paths.ReadCapture(plan.Process.SandboxID); !os.IsNotExist(err) {
		t.Fatalf("intent remains: %v", err)
	}
	if _, err := os.Stat(plan.WritableFiles[0].Destination); !os.IsNotExist(err) {
		t.Fatalf("canceled capture copied disks: %v", err)
	}
}

func TestRecoverCaptureRequiresIntentAndExactGeneration(t *testing.T) {
	for _, test := range []struct {
		name       string
		intent     bool
		generation uint64
		state      string
		failResume bool
		wantResume bool
		wantError  bool
	}{
		{name: "unowned pause", generation: 3, state: "Paused"},
		{name: "interrupted pause", intent: true, generation: 3, state: "Paused", wantResume: true},
		{name: "already resumed", intent: true, generation: 3, state: "Running"},
		{name: "wrong generation", intent: true, generation: 4, state: "Paused", wantError: true},
		{name: "failed resume", intent: true, generation: 3, state: "Paused", failResume: true, wantResume: true, wantError: true},
		{name: "unexpected VM state", intent: true, generation: 3, state: "Created", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var resumed atomic.Bool
			driver, plan := captureTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/vm.info" {
					_, _ = fmt.Fprintf(w, `{"state":%q}`, test.state)
					return
				}
				if r.URL.Path != "/api/v1/vm.resume" {
					t.Errorf("unexpected API %s", r.URL.Path)
				}
				resumed.Store(true)
				if test.failResume {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			if test.intent {
				if err := driver.paths.WriteCapture(vmm.CaptureIntent{Process: plan.Process, Destination: plan.Destination, Action: vmm.CaptureResume}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := driver.RecoverCapture(t.Context(), plan.Process.SandboxID, test.generation)
			if errors.Is(err, vmm.ErrCaptureRecoveryPending) != test.wantError {
				t.Fatalf("recovery = %v", err)
			}
			if resumed.Load() != test.wantResume {
				t.Fatalf("resume = %t", resumed.Load())
			}
			_, intentErr := driver.paths.ReadCapture(plan.Process.SandboxID)
			if test.wantError && intentErr != nil || !test.wantError && !os.IsNotExist(intentErr) {
				t.Fatalf("journal after recovery = %v", intentErr)
			}
		})
	}
}

func TestHibernateCommittedStopSurvivesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var resumed atomic.Bool
	driver, plan := captureTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/vm.info" {
			_, _ = fmt.Fprint(w, `{"state":"Running"}`)
			return
		}
		if r.URL.Path == "/api/v1/vm.resume" {
			resumed.Store(true)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := driver.Hibernate(ctx, plan, func() error { cancel(); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("hibernate = %v", err)
	}
	intent, err := driver.paths.ReadCapture(plan.Process.SandboxID)
	if err != nil || intent.Action != vmm.CaptureStop {
		t.Fatalf("stop intent = %+v, %v", intent, err)
	}
	if _, err := driver.RecoverCapture(t.Context(), plan.Process.SandboxID, plan.Process.Generation); err != nil {
		t.Fatal(err)
	}
	if resumed.Load() {
		t.Fatal("persisted hibernation was resumed")
	}
	if alive, err := verifyProcess(plan.Process); err != nil || alive {
		t.Fatalf("VMM still alive=%t, %v", alive, err)
	}
}

func TestPauseFailureAfterApplyingPauseIsRecovered(t *testing.T) {
	var state atomic.Value
	state.Store("Running")
	driver, plan := captureTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vm.info":
			_, _ = fmt.Fprintf(w, `{"state":%q}`, state.Load())
		case "/api/v1/vm.pause":
			state.Store("Paused")
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/v1/vm.resume":
			state.Store("Running")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	if err := driver.Snapshot(t.Context(), plan); err == nil {
		t.Fatal("pause failure was lost")
	}
	if state.Load() != "Running" {
		t.Fatal("guest remained paused after a failed acknowledgement")
	}
	if _, err := driver.paths.ReadCapture(plan.Process.SandboxID); !os.IsNotExist(err) {
		t.Fatalf("recovered pause intent remains: %v", err)
	}
}

func TestCaptureRecoveryRetriesAfterControllerRestart(t *testing.T) {
	var failResume atomic.Bool
	failResume.Store(true)
	driver, plan := captureTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/vm.info" {
			_, _ = fmt.Fprint(w, `{"state":"Paused"}`)
			return
		}
		if r.URL.Path != "/api/v1/vm.resume" {
			t.Errorf("unexpected API %s", r.URL.Path)
		}
		if failResume.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := driver.paths.WriteCapture(vmm.CaptureIntent{Process: plan.Process, Destination: plan.Destination, Action: vmm.CaptureResume}); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.RecoverCapture(t.Context(), plan.Process.SandboxID, 3); !errors.Is(err, vmm.ErrCaptureRecoveryPending) {
		t.Fatalf("first recovery = %v", err)
	}
	failResume.Store(false)
	restarted := &Driver{paths: driver.paths, scopes: driver.scopes}
	if _, err := restarted.RecoverCapture(t.Context(), plan.Process.SandboxID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.paths.ReadCapture(plan.Process.SandboxID); !os.IsNotExist(err) {
		t.Fatalf("settled intent remains: %v", err)
	}
}
