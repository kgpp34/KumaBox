//go:build linux

package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

func TestCommandLineMatchesDistinguishesPendingIdentity(t *testing.T) {
	const socket = "/run/kumabox/api.sock"
	if match, err := commandLineMatches(nil, "cloud-hypervisor", socket); match || !errors.Is(err, errProcessIdentityPending) {
		t.Fatalf("empty command line = %v, %v; want pending identity", match, err)
	}
	if match, err := commandLineMatches([]byte("cloud-hypervisor\x00--api-socket\x00"+socket+"\x00"), "cloud-hypervisor", socket); err != nil || !match {
		t.Fatalf("complete command line = %v, %v; want match", match, err)
	}
	if match, err := commandLineMatches([]byte("other-vmm\x00--api-socket\x00"+socket+"\x00"), "cloud-hypervisor", socket); err != nil || match {
		t.Fatalf("foreign command line = %v, %v; want mismatch", match, err)
	}
}

func TestProcessIdentityHelper(t *testing.T) {
	if os.Getenv("KUMABOX_PROCESS_HELPER") != "1" {
		return
	}
	if os.Getenv("KUMABOX_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	if err := os.WriteFile(os.Getenv("KUMABOX_READY_FILE"), []byte("ready"), 0o600); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestConfigureProcessPlacesChildInPreparedCgroup(t *testing.T) {
	scope, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close() //nolint:errcheck
	command := exec.Command("true")
	configureProcess(command, scope)
	if command.SysProcAttr == nil || !command.SysProcAttr.Setpgid || !command.SysProcAttr.UseCgroupFD || command.SysProcAttr.CgroupFD != int(scope.Fd()) {
		t.Fatalf("process attributes = %+v", command.SysProcAttr)
	}
}

func TestCaptureAndVerifyProcessIdentity(t *testing.T) {
	command, wait := startProcessHelper(t, false, "/run/kumabox/api.sock")
	process, err := captureProcess(
		command.Process.Pid,
		types.SandboxID("123e4567-e89b-42d3-a456-426614174000"),
		3,
		filepath.Base(os.Args[0]),
		"/run/kumabox/api.sock",
	)
	if err != nil {
		t.Fatal(err)
	}
	if match, err := verifyProcess(process); err != nil || !match {
		t.Fatalf("verifyProcess() = %v, %v", match, err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*vmm.Process)
	}{
		{name: "starttime", mutate: func(value *vmm.Process) { value.StartTicks++ }},
		{name: "boot ID", mutate: func(value *vmm.Process) { value.BootID += "-other" }},
		{name: "binary", mutate: func(value *vmm.Process) { value.Binary = "other-vmm" }},
		{name: "API socket", mutate: func(value *vmm.Process) { value.APISocket = "/run/kumabox/other.sock" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := process
			test.mutate(&candidate)
			if match, err := verifyProcess(candidate); err != nil || match {
				t.Fatalf("verifyProcess() = %v, %v", match, err)
			}
		})
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-wait
}

func TestTerminateProcessEscalatesFromTermToKill(t *testing.T) {
	for _, ignoreTerm := range []bool{false, true} {
		name := "TERM"
		if ignoreTerm {
			name = "KILL"
		}
		t.Run(name, func(t *testing.T) {
			command, wait := startProcessHelper(t, ignoreTerm, "/run/kumabox/api.sock")
			process, err := captureProcess(
				command.Process.Pid,
				types.SandboxID("123e4567-e89b-42d3-a456-426614174000"),
				3,
				filepath.Base(os.Args[0]),
				"/run/kumabox/api.sock",
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := terminateProcess(t.Context(), process, 50*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			select {
			case <-wait:
			case <-time.After(time.Second):
				t.Fatal("helper process was not reaped")
			}
		})
	}
}

func startProcessHelper(t *testing.T, ignoreTerm bool, apiSocket string) (*exec.Cmd, <-chan error) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=TestProcessIdentityHelper", "--", "--api-socket", apiSocket)
	command.Env = append(os.Environ(), "KUMABOX_PROCESS_HELPER=1", "KUMABOX_READY_FILE="+ready)
	if ignoreTerm {
		command.Env = append(command.Env, "KUMABOX_IGNORE_TERM=1")
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
		close(wait)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-wait:
		case <-time.After(time.Second):
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return command, wait
		}
		if time.Now().After(deadline) {
			t.Fatal("helper process did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTerminateProcessCancellationDoesNotEscalate(t *testing.T) {
	command, _ := startProcessHelper(t, true, "/run/kumabox/api.sock")
	process, err := captureProcess(command.Process.Pid, "123e4567-e89b-42d3-a456-426614174000", 3, filepath.Base(os.Args[0]), "/run/kumabox/api.sock")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := terminateProcess(ctx, process, time.Minute); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("terminate = %v", err)
	}
	if alive, err := verifyProcess(process); err != nil || !alive {
		t.Fatalf("cancellation killed owned process: alive=%v, err=%v", alive, err)
	}
}

func TestStopShutdownPolicyAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name       string
		force      bool
		powerExit  bool
		powerError bool
		cancel     bool
		mismatch   bool
		want       []string
	}{
		{name: "normal guest exit", powerExit: true, want: []string{"vm.power-button"}},
		{name: "guest timeout escalates", want: []string{"vm.power-button", "vm.shutdown"}},
		{name: "force skips ACPI", force: true, want: []string{"vm.shutdown"}},
		{name: "ACPI unavailable escalates", powerError: true, want: []string{"vm.power-button", "vm.shutdown"}},
		{name: "caller cancellation preserves process", cancel: true, want: []string{"vm.power-button"}},
		{name: "foreign generation is never signalled", mismatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "api.sock")
			command, _ := startProcessHelper(t, !test.powerExit, socket)
			process, err := captureProcess(command.Process.Pid, "123e4567-e89b-42d3-a456-426614174000", 3, filepath.Base(os.Args[0]), socket)
			if err != nil {
				t.Fatal(err)
			}
			if test.mismatch {
				process.StartTicks++
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			var actions []string
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				action := strings.TrimPrefix(r.URL.Path, "/api/v1/")
				mu.Lock()
				actions = append(actions, action)
				mu.Unlock()
				if action == "vm.power-button" {
					switch {
					case test.powerError:
						w.WriteHeader(http.StatusNotImplemented)
						return
					case test.powerExit:
						_ = command.Process.Signal(syscall.SIGTERM)
					case test.cancel:
						cancel()
					}
				}
				w.WriteHeader(http.StatusNoContent)
			})}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			driver := &Driver{stopGrace: 30 * time.Millisecond}
			err = driver.Stop(ctx, process, types.StopOptions{Force: test.force, Timeout: 50 * time.Millisecond})
			if test.cancel {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("stop cancellation = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			got := slices.Clone(actions)
			mu.Unlock()
			if !slices.Equal(got, test.want) {
				t.Fatalf("actions = %v; want %v", got, test.want)
			}
			if test.cancel || test.mismatch {
				if err := command.Process.Signal(syscall.Signal(0)); err != nil {
					t.Fatalf("cancelled stop or foreign identity killed process: %v", err)
				}
			} else if err := command.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("stop returned before process exit: %v", err)
			}
		})
	}
}

func TestSnapshotWaitsForBalloonBeforePausingGuest(t *testing.T) {
	base := t.TempDir()
	paths, err := vmm.NewPaths(storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")})
	if err != nil {
		t.Fatal(err)
	}
	id := types.SandboxID("123e4567-e89b-42d3-a456-426614174000")
	if err := paths.Prepare(id); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "kb-balloon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "api.sock")
	command, _ := startProcessHelper(t, false, socket)
	process, err := captureProcess(command.Process.Pid, id, 3, filepath.Base(os.Args[0]), socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.WriteProcess(process); err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	var paused atomic.Bool
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/vm.info":
			actual := 512 << 20
			if probes.Add(1) >= 7 {
				actual = 384 << 20
			}
			_, _ = fmt.Fprintf(w, `{"state":"Running","memory_actual_size":%d,"config":{"memory":{"size":536870912},"balloon":{"size":134217728}}}`, actual)
		case "/api/v1/vm.pause":
			if probes.Load() < 7 {
				t.Error("guest paused before reclaim completed")
			}
			paused.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	driver := &Driver{paths: paths, scopes: &cleanupScope{}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	plan := vmm.SnapshotPlan{Process: process, Destination: "/capture", WritableFiles: []vmm.SnapshotFile{{Source: "/live/cow.raw", Destination: "/capture/cow.raw"}}}
	if err := driver.pauseForCapture(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if !paused.Load() {
		t.Fatal("settled guest was not paused for capture")
	}
}
