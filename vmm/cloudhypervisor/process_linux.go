//go:build linux

package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netns"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

// platformPreflight verifies that KVM can be opened by the current identity.
func platformPreflight() error {
	device, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/kvm: %w", err)
	}
	return device.Close()
}

// configureProcess makes the VMM independent of the CLI process group and asks
// clone3 to place it in the prepared cgroup before it executes user code.
func configureProcess(command *exec.Cmd, scope *os.File) {
	command.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:     true,
		UseCgroupFD: true,
		CgroupFD:    int(scope.Fd()),
	}
}

// startProcess starts the child in the requested network namespace. setns is
// thread-local, so the caller thread is pinned until the original namespace is
// restored after fork and exec.
func startProcess(command *exec.Cmd, namespacePath string) (returnErr error) {
	if namespacePath == "" {
		return command.Start()
	}
	if !filepath.IsAbs(namespacePath) {
		return errors.New("VMM network namespace path must be absolute")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current network namespace: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, original.Close()) }()
	target, err := netns.GetFromPath(namespacePath)
	if err != nil {
		return fmt.Errorf("open VMM network namespace %s: %w", namespacePath, err)
	}
	defer func() { returnErr = errors.Join(returnErr, target.Close()) }()
	if err := netns.Set(target); err != nil {
		return fmt.Errorf("enter VMM network namespace %s: %w", namespacePath, err)
	}
	defer func() {
		if err := netns.Set(original); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore host network namespace: %w", err))
		}
	}()
	return command.Start()
}

func captureProcess(pid int, id types.SandboxID, generation uint64, binary, apiSocket string) (vmm.Process, error) {
	start, err := processStartTicks(pid)
	if err != nil {
		return vmm.Process{}, err
	}
	bootID, err := hostBootID()
	if err != nil {
		return vmm.Process{}, err
	}
	process := vmm.Process{PID: pid, StartTicks: start, BootID: bootID, SandboxID: id, Generation: generation, Binary: binary, APISocket: apiSocket}
	return process, process.Validate()
}

func identifyProcess(pid int, id types.SandboxID, generation uint64, binary, apiSocket string) (vmm.Process, bool, error) {
	match, err := processCommandMatches(pid, binary, apiSocket)
	if err != nil {
		if !processExists(pid) {
			return vmm.Process{}, false, nil
		}
		return vmm.Process{}, false, err
	}
	if !match {
		return vmm.Process{}, false, nil
	}
	process, err := captureProcess(pid, id, generation, binary, apiSocket)
	return process, err == nil, err
}

func verifyProcess(process vmm.Process) (bool, error) {
	bootID, err := hostBootID()
	if err != nil {
		return false, err
	}
	if bootID != process.BootID {
		return false, nil
	}
	start, err := processStartTicks(process.PID)
	if err != nil {
		if !processExists(process.PID) {
			return false, nil
		}
		return false, err
	}
	if start != process.StartTicks {
		return false, nil
	}
	return processCommandMatches(process.PID, process.Binary, process.APISocket)
}

// terminateProcess opens a pidfd before identity checks, closing the PID-reuse
// race between verification and signal delivery.
func terminateProcess(ctx context.Context, process vmm.Process, grace time.Duration) error {
	handle, err := os.FindProcess(process.PID)
	if err != nil {
		return err
	}
	defer handle.Release() //nolint:errcheck // releasing the pidfd cannot change process state
	match, err := verifyProcess(process)
	if err != nil {
		return err
	}
	if !match {
		return nil
	}
	if err := handle.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	if waitProcess(ctx, handle, grace) == nil {
		return nil
	}
	if err := handle.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return waitProcess(ctx, handle, time.Second)
}

func waitProcess(ctx context.Context, process *os.Process, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := process.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return nil
		} else if err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out waiting for VMM process exit")
		case <-ticker.C:
		}
	}
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func processCommandMatches(pid int, binary, apiSocket string) (bool, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)) //nolint:gosec // pid is a positive kernel identity
	if err != nil {
		return false, err
	}
	fields := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(fields) == 0 || filepath.Base(fields[0]) != binary {
		return false, nil
	}
	matches := 0
	for index := 1; index+1 < len(fields); index++ {
		if fields[index] == "--api-socket" && fields[index+1] == apiSocket {
			matches++
		}
	}
	return matches == 1, nil
}

func processStartTicks(pid int) (uint64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) //nolint:gosec // pid is a positive kernel identity
	if err != nil {
		return 0, err
	}
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return 0, errors.New("process stat has no command terminator")
	}
	fields := strings.Fields(text[end+1:])
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex {
		return 0, errors.New("process stat omitted starttime")
	}
	start, err := strconv.ParseUint(fields[startTimeIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process starttime: %w", err)
	}
	return start, nil
}

func hostBootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", errors.New("host boot ID is empty")
	}
	return value, nil
}
