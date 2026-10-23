//go:build !linux

package vmm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/kumabox/kumabox/types"
)

var errLinuxRequired = errors.New("VMM lifecycle requires Linux")

func PlatformPreflight() error { return errLinuxRequired }

func ConfigureProcess(*exec.Cmd, *os.File) {}

func StartProcess(*exec.Cmd, string) error { return errLinuxRequired }

func CaptureProcess(int, types.SandboxID, uint64, string, string) (Process, error) {
	return Process{}, errLinuxRequired
}

func IdentifyProcess(int, types.SandboxID, uint64, string, string) (Process, bool, error) {
	return Process{}, false, errLinuxRequired
}

func VerifyProcess(Process) (bool, error) { return false, errLinuxRequired }

func TerminateProcess(context.Context, Process, time.Duration) error { return errLinuxRequired }

func WaitProcessExit(context.Context, Process, time.Duration) error { return errLinuxRequired }
