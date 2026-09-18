//go:build !linux

package agent

import (
	"os"
	"os/exec"
)

func configureProcess(*exec.Cmd) {}

func processExitCode(state *os.ProcessState) int {
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	return 1
}
