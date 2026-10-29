//go:build !linux

package agent

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/kumabox/kumabox/internal/types"
)

func configureProcess(*exec.Cmd) {}

func configureIdentity(_ *exec.Cmd, name string) (map[string]string, error) {
	if name != "" {
		return nil, errors.New("guest user selection requires Linux")
	}
	return nil, nil
}

func waitManagedProcess(command *exec.Cmd, gate *sync.Mutex, exited *bool) error {
	err := command.Wait()
	gate.Lock()
	*exited = true
	gate.Unlock()
	return err
}

func signalManagedProcess(command *exec.Cmd, signal types.ProcessSignal) error {
	switch signal {
	case types.ProcessSignalKILL:
		return command.Process.Kill()
	case types.ProcessSignalINT:
		return command.Process.Signal(os.Interrupt)
	case types.ProcessSignalHUP:
		return command.Process.Signal(syscall.SIGHUP)
	default:
		return command.Process.Signal(syscall.SIGTERM)
	}
}

func processExitCode(state *os.ProcessState) int {
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	return 1
}
