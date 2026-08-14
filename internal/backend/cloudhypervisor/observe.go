package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/vm"
)

const backendObserveTimeout = 500 * time.Millisecond

func ObserveVM(rec *vm.VMRecord) vm.Observation {
	now := time.Now().UTC()
	if rec == nil {
		return observation(vm.ObservedStateUnknown, "VM record is nil", now)
	}

	switch rec.State {
	case vm.StateCreated:
		return observation(vm.ObservedStateCreated, "VM has not been started", now)
	case vm.StateStopped:
		return observation(vm.ObservedStateStopped, "VM is stopped", now)
	case vm.StateError:
		if rec.Error != "" {
			return observation(vm.ObservedStateFailed, rec.Error, now)
		}
		return observation(vm.ObservedStateFailed, "VM is recorded in error state", now)
	case vm.StateRunning, vm.StatePaused:
	default:
		return observation(vm.ObservedStateUnknown, "unrecognized persisted state "+string(rec.State), now)
	}

	pid := rec.PID
	apiSocket := rec.APISocket
	binary := ""
	if cfg, err := readRenderedConfig(rec.Config); err == nil {
		binary = cfg.Binary
		if apiSocket == "" {
			apiSocket = cfg.APISocket
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return observation(vm.ObservedStateUnknown, fmt.Sprintf("read backend config: %v", err), now)
	}

	if pid <= 0 {
		return observation(vm.ObservedStateUnknown, "running record has no pid", now)
	}
	if !processAlive(pid) {
		return observation(vm.ObservedStateStopped, fmt.Sprintf("process %d is not alive", pid), now)
	}
	if binary != "" && apiSocket != "" {
		matched, reason := verifyProcessIdentity(pid, binary, apiSocket)
		if !matched {
			return observation(vm.ObservedStateUnknown, reason, now)
		}
	}
	if apiSocket == "" {
		return observation(vm.ObservedStateUnknown, "running record has no API socket", now)
	}
	ctx, cancel := context.WithTimeout(context.Background(), backendObserveTimeout)
	defer cancel()
	info, err := queryVMInfo(ctx, apiSocket, backendObserveTimeout)
	if err != nil {
		return observation(vm.ObservedStateUnknown, fmt.Sprintf("API state check failed: %v", err), now)
	}
	switch strings.ToLower(info.State) {
	case "running":
		return observation(vm.ObservedStateRunning, "process identity and backend state are healthy", now)
	case "paused":
		return observation(vm.ObservedStatePaused, "process identity is healthy and backend is paused", now)
	default:
		return observation(vm.ObservedStateUnknown, "backend reported state "+info.State, now)
	}
}

func observation(state vm.ObservedState, reason string, checkedAt time.Time) vm.Observation {
	return vm.Observation{
		State:     state,
		Reason:    reason,
		CheckedAt: checkedAt,
	}
}

func readRenderedConfig(path string) (*Config, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse Cloud Hypervisor config: %w", err)
	}
	return &cfg, nil
}

func verifyProcessIdentity(pid int, binary string, apiSocket string) (bool, string) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)) //nolint:gosec
	if err != nil {
		return false, fmt.Sprintf("cannot verify process identity for pid %d: %v", pid, err)
	}

	cmdline := string(data)
	binaryName := filepath.Base(binary)
	if !strings.Contains(cmdline, binaryName) {
		return false, fmt.Sprintf("pid %d command line does not contain %q", pid, binaryName)
	}
	if !strings.Contains(cmdline, apiSocket) {
		return false, fmt.Sprintf("pid %d command line does not contain API socket %q", pid, apiSocket)
	}
	return true, ""
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
