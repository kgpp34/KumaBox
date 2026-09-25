package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/vmm"
)

var (
	_ vmm.Restorer         = (*Driver)(nil)
	_ vmm.RestoreValidator = (*Driver)(nil)
)

// ValidateRestore checks the native files Cloud Hypervisor requires before a
// caller stops the current sandbox process.
func (*Driver) ValidateRestore(_ context.Context, directory string) error {
	for _, name := range []string{"config.json", "state.json"} {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("snapshot %s is not a nonempty regular file", name)
		}
	}
	raw, err := os.ReadFile(filepath.Join(directory, "config.json")) //nolint:gosec // managed snapshot path
	if err != nil {
		return err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil || len(config) == 0 {
		return errors.Join(err, errors.New("snapshot config.json is empty or invalid"))
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "memory-range") {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() && info.Size() > 0 {
				return nil
			}
		}
	}
	return errors.New("snapshot has no nonempty memory-range file")
}

// Restore launches an API-only process in the target sandbox's cgroup and
// namespace, loads native state, resumes the VM, and proves readiness.
//
//	runtime dirs -> API-only process -> vm.restore -> vm.resume -> Running
func (d *Driver) Restore(ctx context.Context, plan vmm.RestorePlan) (result vmm.Process, returnErr error) {
	if err := plan.Validate(); err != nil {
		return vmm.Process{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if err := d.Preflight(); err != nil {
		return vmm.Process{}, err
	}
	if err := d.paths.Prepare(plan.SandboxID); err != nil {
		return vmm.Process{}, err
	}
	var command *exec.Cmd
	defer func() {
		if returnErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.abortGrace+time.Second)
		defer cancel()
		switch {
		case result.PID > 0:
			returnErr = errors.Join(returnErr, d.Abort(cleanupCtx, result))
		case command != nil && command.Process != nil:
			returnErr = errors.Join(returnErr, command.Process.Kill(), command.Wait(), d.scopes.Remove(cleanupCtx, plan.SandboxID), d.paths.Clear(plan.SandboxID))
		default:
			returnErr = errors.Join(returnErr, d.scopes.Remove(cleanupCtx, plan.SandboxID), d.paths.Clear(plan.SandboxID))
		}
	}()
	apiSocket, _ := d.paths.APISocket(plan.SandboxID)
	args := []string{"--api-socket", apiSocket}
	if err := d.paths.WriteCmdline(plan.SandboxID, diagnosticCommand(d.binary, args)); err != nil {
		return vmm.Process{}, err
	}
	scope, err := d.scopes.Prepare(ctx, plan.SandboxID, plan.CPUs)
	if err != nil {
		return vmm.Process{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, scope.Close()) }()
	logPath, _ := d.paths.LogFile(plan.SandboxID)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // managed path
	if err != nil {
		return vmm.Process{}, fmt.Errorf("open VMM log: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, logFile.Close()) }()
	command = exec.Command(d.binary, args...) //nolint:gosec // configured executable, no shell
	command.Stdout, command.Stderr = logFile, logFile
	configureProcess(command, scope)
	if err := startProcess(command, plan.Network.Namespace); err != nil {
		return vmm.Process{}, fmt.Errorf("exec cloud-hypervisor restore process: %w", err)
	}
	result, err = captureProcess(command.Process.Pid, plan.SandboxID, plan.Generation, filepath.Base(d.binary), apiSocket)
	if err != nil {
		return result, fmt.Errorf("capture restore process identity: %w", err)
	}
	if err := d.paths.WriteProcess(result); err != nil {
		return result, fmt.Errorf("persist restore process identity: %w", err)
	}
	go func() { _ = command.Wait() }()
	if err := d.waitAPISocket(ctx, result); err != nil {
		return result, err
	}
	payload, err := json.Marshal(map[string]string{"source_url": "file://" + plan.SnapshotDir})
	if err != nil {
		return result, err
	}
	if err := d.snapshotAction(ctx, apiSocket, "vm.restore", payload, snapshotTimeout); err != nil {
		return result, fmt.Errorf("restore cloud-hypervisor state: %w", err)
	}
	if err := d.snapshotAction(ctx, apiSocket, "vm.resume", nil, d.startupTimeout); err != nil {
		return result, fmt.Errorf("resume restored cloud-hypervisor: %w", err)
	}
	if err := d.WaitReady(ctx, result); err != nil {
		return result, err
	}
	return result, nil
}

func (d *Driver) waitAPISocket(ctx context.Context, process vmm.Process) error {
	deadline := time.NewTimer(d.startupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("unix", process.APISocket, probeInterval)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		located, exists, locateErr := d.Locate(ctx, process.SandboxID, process.Generation)
		if locateErr != nil {
			return locateErr
		}
		if !exists || located.PID != process.PID || located.StartTicks != process.StartTicks {
			return errors.New("cloud-hypervisor restore process exited before its API socket became ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out waiting for cloud-hypervisor restore API socket")
		case <-ticker.C:
		}
	}
}
