//go:build linux

package agent

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcess places the guest command in its own process group so
// cancellation cannot leave background descendants behind.
func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}

// processExitCode maps signal termination to the conventional shell status
// 128+signal so the host CLI can preserve meaningful guest results.
func processExitCode(state *os.ProcessState) int {
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return 1
}
