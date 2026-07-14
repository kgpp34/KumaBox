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

	"github.com/kumabox/kumabox/internal/vmstore"
)

func ObserveVM(rec *vmstore.VMRecord) vmstore.Observation {
	now := time.Now().UTC()
	if rec == nil {
		return observation(vmstore.ObservedStateUnknown, "VM record is nil", now)
	}

	switch rec.State {
	case vmstore.StateCreated:
		return observation(vmstore.ObservedStateCreated, "VM has not been started", now)
	case vmstore.StateStopped:
		return observation(vmstore.ObservedStateStopped, "VM is stopped", now)
	case vmstore.StateError:
		if rec.Error != "" {
			return observation(vmstore.ObservedStateFailed, rec.Error, now)
		}
		return observation(vmstore.ObservedStateFailed, "VM is recorded in error state", now)
	case vmstore.StateRunning, vmstore.StatePaused:
	default:
		return observation(vmstore.ObservedStateUnknown, "unrecognized persisted state "+string(rec.State), now)
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
		return observation(vmstore.ObservedStateUnknown, fmt.Sprintf("read backend config: %v", err), now)
	}

	if pid <= 0 {
		return observation(vmstore.ObservedStateUnknown, "running record has no pid", now)
	}
	if !processAlive(pid) {
		return observation(vmstore.ObservedStateStopped, fmt.Sprintf("process %d is not alive", pid), now)
	}
	if binary != "" && apiSocket != "" {
		matched, reason := verifyProcessIdentity(pid, binary, apiSocket)
		if !matched {
			return observation(vmstore.ObservedStateUnknown, reason, now)
		}
	}
	if apiSocket == "" {
		return observation(vmstore.ObservedStateUnknown, "running record has no API socket", now)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	info, err := queryVMInfo(ctx, apiSocket, 500*time.Millisecond)
	if err != nil {
		return observation(vmstore.ObservedStateUnknown, fmt.Sprintf("API state check failed: %v", err), now)
	}
	switch strings.ToLower(info.State) {
	case "running":
		return observation(vmstore.ObservedStateRunning, "process identity and backend state are healthy", now)
	case "paused":
		return observation(vmstore.ObservedStatePaused, "process identity is healthy and backend is paused", now)
	default:
		return observation(vmstore.ObservedStateUnknown, "backend reported state "+info.State, now)
	}
}

func observation(state vmstore.ObservedState, reason string, checkedAt time.Time) vmstore.Observation {
	return vmstore.Observation{
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
