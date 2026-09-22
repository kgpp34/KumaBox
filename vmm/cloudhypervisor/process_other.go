//go:build !linux

package cloudhypervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

var errLinuxRequired = errors.New("cloud hypervisor lifecycle requires Linux")

func platformPreflight() error { return errLinuxRequired }

func configureProcess(*exec.Cmd, *os.File) {}

func startProcess(*exec.Cmd, string) error { return errLinuxRequired }

func captureProcess(int, types.SandboxID, uint64, string, string) (vmm.Process, error) {
	return vmm.Process{}, errLinuxRequired
}

func identifyProcess(int, types.SandboxID, uint64, string, string) (vmm.Process, bool, error) {
	return vmm.Process{}, false, errLinuxRequired
}

func verifyProcess(vmm.Process) (bool, error) { return false, errLinuxRequired }

func terminateProcess(context.Context, vmm.Process, time.Duration) error { return errLinuxRequired }
