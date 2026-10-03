//go:build linux

package cli

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/types"
)

func TestPIDFDWatcherReportsProcessExit(t *testing.T) {
	watcher, err := newExitWatcher()
	if err != nil {
		t.Skipf("epoll unavailable: %v", err)
	}
	defer func() { _ = watcher.Close() }()
	command := exec.CommandContext(t.Context(), "sh", "-c", "sleep 0.1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Wait() }()
	err = watcher.Sync([]core.SandboxStatus{{
		Sandbox: types.Sandbox{ID: "123e4567-e89b-42d3-a456-426614174000", Generation: 4},
		PID:     command.Process.Pid,
	}})
	if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) {
		t.Skipf("pidfd unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-watcher.Events():
	case <-time.After(3 * time.Second):
		t.Fatal("process exit did not wake pidfd watcher")
	}
}
