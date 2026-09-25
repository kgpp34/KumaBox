//go:build linux

package cloudhypervisor

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

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
