// Package firecracker implements the direct-boot Firecracker process adapter.
// Durable sandbox state, image ownership and CNI allocation remain in core.
package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/cgroup"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

const probeInterval = 5 * time.Millisecond

// Options configures the executable and bounded host lifecycle policy.
type Options = vmm.ProcessOptions

// Driver owns Firecracker process/API adaptation, without lifecycle metadata.
type Driver struct {
	// paths fences sockets, identity and logs to the owning sandbox.
	paths vmm.Paths
	// scopes applies host CPU policy before the process executes.
	scopes *cgroup.Manager
	// options freezes executable and bounded wait policy.
	options Options
}

var _ vmm.Backend = (*Driver)(nil)

// New validates policy without requiring Firecracker to be installed unless used.
func New(paths vmm.Paths, scopes *cgroup.Manager, options Options) (*Driver, error) {
	if scopes == nil {
		return nil, errors.New("firecracker cgroup manager is required")
	}
	resolved, err := options.Resolve("firecracker")
	if err != nil {
		return nil, err
	}
	options = resolved
	return &Driver{paths: paths, scopes: scopes, options: options}, nil
}

// Type is the durable identity used to route every later sandbox operation.
func (*Driver) Type() types.VMMType { return types.VMMFirecracker }

// NetworkQueues selects the single RX/TX pair supported by Firecracker.
func (*Driver) NetworkQueues(uint32) int { return 2 }

// Preflight requires Linux, accessible KVM, and the configured executable.
func (d *Driver) Preflight() error {
	if runtime.GOARCH != "amd64" {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("this firecracker backend currently requires Linux amd64"))
	}
	if err := vmm.PlatformPreflight(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, err)
	}
	if _, err := exec.LookPath(d.options.Binary); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, err)
	}
	return d.paths.Ensure()
}

// Launch gives the monitor one immutable config, places it directly in its
// cgroup, records PID/start/boot identity, and waits for the exact VM to run.
//
//	plan -> cached ELF + config -> exec in CNI namespace -> identity
//	                                                        |
//	                                                   API Running
//
// Any failure terminates only the captured process before clearing runtime files.
func (d *Driver) Launch(ctx context.Context, plan vmm.LaunchPlan) (result vmm.Process, returnErr error) {
	socket, err := d.paths.Vsock(plan.SandboxID)
	if err != nil {
		return result, err
	}
	cfg, err := buildConfig(plan, plan.Kernel, socket)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := d.Preflight(); err != nil {
		return result, err
	}
	if err := d.paths.Prepare(plan.SandboxID); err != nil {
		return result, err
	}
	defer func() {
		if returnErr == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.options.AbortGrace+time.Second)
		defer cancel()
		if result.PID > 0 {
			returnErr = errors.Join(returnErr, d.Abort(cleanup, result))
		} else {
			returnErr = errors.Join(returnErr, d.Cleanup(cleanup, plan.SandboxID))
		}
	}()
	cfg.Boot.Kernel, err = prepareKernel(ctx, d.paths.KernelCache(), plan.Kernel)
	if err != nil {
		return result, err
	}
	dir, _ := d.paths.RunDir(plan.SandboxID)
	configPath := filepath.Join(dir, "firecracker.json")
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return result, err
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		return result, err
	} //nolint:gosec // private validated runtime directory
	api, _ := d.paths.APISocket(plan.SandboxID)
	args := []string{"--api-sock", api, "--config-file", configPath, "--id", plan.SandboxID.String(), "--level", "Warning"}
	scope, err := d.scopes.Prepare(ctx, plan.SandboxID, plan.CPUs, plan.CPUPolicy)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, scope.Close()) }()
	result, err = d.launchProcess(ctx, processLaunch{
		sandboxID: plan.SandboxID, generation: plan.Generation,
		args: args, namespace: plan.Network.Namespace, scope: scope,
	})
	if err != nil {
		return result, err
	}
	if err := d.WaitReady(ctx, result); err != nil {
		return result, err
	}
	return result, nil
}

// processLaunch owns only process-level inputs; cold boot and native restore
// prepare their configuration and CPU policy before sharing this launch path.
type processLaunch struct {
	sandboxID  types.SandboxID
	generation uint64
	args       []string
	namespace  string
	scope      *os.File
	// root selects a private filesystem view for native path redirection only.
	root string
}

// launchProcess shares log/PTY ownership, exact identity, reaping and relay
// startup. Its caller owns runtime cleanup and any post-exec failure rollback.
func (d *Driver) launchProcess(ctx context.Context, launch processLaunch) (result vmm.Process, returnErr error) {
	diagnostic := []string{strconv.Quote(d.options.Binary)}
	for _, arg := range launch.args {
		diagnostic = append(diagnostic, strconv.Quote(arg))
	}
	if err := d.paths.WriteCmdline(launch.sandboxID, strings.Join(diagnostic, " ")); err != nil {
		return result, err
	}
	logPath, _ := d.paths.LogFile(launch.sandboxID)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // managed path
	if err != nil {
		return result, fmt.Errorf("open VMM log: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, log.Close()) }()
	consoleSocket, _ := d.paths.ConsoleSocket(launch.sandboxID)
	console, err := prepareConsole(consoleSocket)
	if err != nil {
		return result, err
	}
	defer console.close()
	command := exec.Command(d.options.Binary, launch.args...) //nolint:gosec // configured executable, no shell
	command.Stdin, command.Stdout, command.Stderr = console.slave, console.slave, log
	vmm.ConfigureProcess(command, launch.scope)
	command.SysProcAttr.Chroot = launch.root
	if err := vmm.StartProcess(command, launch.namespace); err != nil {
		// Namespace restoration can fail after exec has already succeeded.
		if command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		return result, fmt.Errorf("exec firecracker: %w", err)
	}
	// Capture failure still needs a reaper even before a durable identity exists.
	api, _ := d.paths.APISocket(launch.sandboxID)
	result, err = vmm.CaptureProcess(command.Process.Pid, launch.sandboxID, launch.generation, filepath.Base(d.options.Binary), api)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return result, err
	}
	go func() { _ = command.Wait() }()
	if err := d.paths.WriteProcess(result); err != nil {
		return result, err
	}
	if err := d.startConsoleRelay(ctx, result, console, log); err != nil {
		return result, fmt.Errorf("start console relay: %w", err)
	}
	return result, nil
}

// Locate verifies process generation and recovers only from the owned cgroup.
func (d *Driver) Locate(_ context.Context, id types.SandboxID, generation uint64) (vmm.Process, bool, error) {
	return vmm.LocateProcess(d.paths, d.scopes, id, generation, d.options.Binary)
}

// Observe distinguishes an absent process from a VM still configuring devices.
func (d *Driver) Observe(ctx context.Context, id types.SandboxID, generation uint64) (vmm.Observation, error) {
	process, alive, err := d.Locate(ctx, id, generation)
	if errors.Is(err, vmm.ErrProcessIdentityPending) {
		return vmm.Observation{State: vmm.ProcessStarting, Process: process}, nil
	}
	if err != nil {
		return vmm.Observation{}, err
	}
	if !alive {
		return vmm.Observation{State: vmm.ProcessAbsent}, nil
	}
	var info struct {
		State string `json:"state"`
	}
	err = apiRequest(ctx, process.APISocket, http.MethodGet, "/", nil, &info)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, context.DeadlineExceeded) {
			return vmm.Observation{State: vmm.ProcessStarting, Process: process}, nil
		}
		return vmm.Observation{}, err
	}
	if info.State == "Running" {
		return vmm.Observation{State: vmm.ProcessRunning, Process: process}, nil
	}
	if info.State != "Not started" && info.State != "Paused" {
		return vmm.Observation{}, fmt.Errorf("firecracker returned unexpected state %q", info.State)
	}
	return vmm.Observation{State: vmm.ProcessStarting, Process: process}, nil
}

// WaitReady bounds readiness and rejects a replaced process even with the same ID.
func (d *Driver) WaitReady(ctx context.Context, process vmm.Process) error {
	ctx, cancel := context.WithTimeout(ctx, d.options.StartupTimeout)
	defer cancel()
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		obs, err := d.Observe(ctx, process.SandboxID, process.Generation)
		if err != nil {
			return err
		}
		if obs.State == vmm.ProcessAbsent {
			return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("firecracker exited before reaching Running; inspect vmm.log"))
		}
		if obs.Process != process {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("firecracker process identity changed during startup"))
		}
		if obs.State == vmm.ProcessRunning {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for firecracker Running: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Abort kills an unsuccessful launch before clearing its runtime directory.
func (d *Driver) Abort(ctx context.Context, process vmm.Process) error {
	if err := process.Validate(); err != nil {
		return err
	}
	if err := vmm.TerminateProcess(ctx, process, d.options.AbortGrace); err != nil {
		return err
	}
	return d.Cleanup(ctx, process.SandboxID)
}

// Stop sends CtrlAltDel for graceful shutdown, then enforces the owned-process
// TERM/KILL policy. Guest kernels without i8042 require the timeout fallback.
func (d *Driver) Stop(ctx context.Context, process vmm.Process, options types.StopOptions) error {
	if err := options.Validate(); err != nil {
		return err
	}
	if err := process.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	alive, err := vmm.VerifyProcess(process)
	if err != nil || !alive {
		return err
	}
	if !options.Force {
		timeout := options.Timeout
		if timeout == 0 {
			timeout = types.DefaultStopTimeout
		}
		shutdown, cancel := context.WithTimeout(ctx, timeout)
		action := struct {
			Type string `json:"action_type"`
		}{"SendCtrlAltDel"}
		err := apiRequest(shutdown, process.APISocket, http.MethodPut, "/actions", action, nil)
		if err == nil {
			err = vmm.WaitProcessExit(shutdown, process, timeout)
		}
		cancel()
		if err == nil {
			return nil
		}
	}
	return vmm.TerminateProcess(ctx, process, d.options.StopGrace)
}

// DialVsock preserves the guest agent protocol and validates ownership before
// and after the hybrid handshake, just like the other monitor adapter.
func (d *Driver) DialVsock(ctx context.Context, process vmm.Process, port uint32) (io.ReadWriteCloser, error) {
	if err := process.Validate(); err != nil {
		return nil, err
	}
	alive, err := vmm.VerifyProcess(process)
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, errors.New("firecracker process is absent")
	}
	socket, err := d.paths.Vsock(process.SandboxID)
	if err != nil {
		return nil, err
	}
	connection, err := vmm.DialHybridVsock(ctx, socket, port)
	if err != nil {
		return nil, err
	}
	alive, err = vmm.VerifyProcess(process)
	if err != nil || !alive {
		_ = connection.Close()
		if err == nil {
			err = errors.New("firecracker exited while connecting vsock")
		}
		return nil, err
	}
	return connection, nil
}

// Cleanup removes runtime files only when the cgroup is proven empty.
func (d *Driver) Cleanup(ctx context.Context, id types.SandboxID) error {
	if err := d.scopes.Remove(ctx, id); err != nil {
		return err
	}
	return d.paths.Clear(ctx, id)
}

// Logs streams durable serial and monitor diagnostics.
func (d *Driver) Logs(ctx context.Context, id types.SandboxID, options vmm.LogOptions, out io.Writer) error {
	return d.paths.Logs(ctx, id, options, out)
}

// RemoveLogs runs only when core removes the sandbox record.
func (d *Driver) RemoveLogs(ctx context.Context, id types.SandboxID) error {
	return d.paths.RemoveLogs(ctx, id)
}
