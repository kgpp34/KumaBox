//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/vmm"
)

func TestStartupNeverAcceptsPendingProcessIdentity(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()  //nolint:errcheck
	defer writer.Close() //nolint:errcheck
	socket := filepath.Join(t.TempDir(), "api.sock")
	command := exec.Command("/bin/sh", "-c", "read line", "--api-sock", socket)
	command.Stdin = input
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	process, err := vmm.CaptureProcess(command.Process.Pid, "123e4567-e89b-42d3-a456-426614174000", 1, "sh", socket)
	if err != nil {
		t.Fatal(err)
	}
	driver := &Driver{options: Options{StartupTimeout: 30 * time.Millisecond}}
	if err := driver.waitProcessIdentity(t.Context(), process); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Leave the child unreaped: its empty command line exercises the same
	// pending-identity result as the exec gap, without a scheduler-dependent race.
	limit := time.Now().Add(time.Second)
	for {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", process.PID))
		if err == nil && len(raw) == 0 {
			break
		}
		if time.Now().After(limit) {
			t.Fatal("child did not enter exiting state")
		}
		time.Sleep(time.Millisecond)
	}
	if err := driver.waitProcessIdentity(t.Context(), process); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending identity was accepted or lost its deadline: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed child exited successfully")
	}
	if err := driver.waitProcessIdentity(t.Context(), process); err == nil {
		t.Fatal("absent process was accepted")
	}
}
