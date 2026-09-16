// Package cloudhypervisor adapts Cloud Hypervisor's process and Unix HTTP API
// to KumaBox launch plans. It owns VMM arguments, process identity, readiness,
// and failed-launch termination; core owns durable sandbox state transitions.
package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/cgroup"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

const (
	defaultStartupTimeout = 10 * time.Second
	probeInterval         = 50 * time.Millisecond
	probeTimeout          = 500 * time.Millisecond
	abortGrace            = 3 * time.Second
	maxAPIResponse        = 1 << 20
)

// scopeManager is the cgroup capability consumed by this process adapter.
type scopeManager interface {
	Prepare(context.Context, types.SandboxID, uint32) (*os.File, error)
	PIDs(types.SandboxID) ([]int, error)
	Remove(context.Context, types.SandboxID) error
}

// Options configures the executable and bounded readiness wait.
type Options struct {
	// Binary is an executable name or absolute path; empty selects cloud-hypervisor.
	Binary string
	// StartupTimeout bounds process/API readiness; zero selects ten seconds.
	StartupTimeout time.Duration
}

// Driver launches and observes Cloud Hypervisor processes.
type Driver struct {
	// paths owns runtime identity, sockets, command diagnostics, and logs.
	paths vmm.Paths
	// scopes places every child directly into a per-sandbox cgroup.
	scopes scopeManager
	// binary is resolved by exec only during host preflight.
	binary string
	// startupTimeout bounds API readiness for new and recovered starts.
	startupTimeout time.Duration
}

// New constructs a driver without probing host capabilities.
func New(paths vmm.Paths, scopes *cgroup.Manager, options Options) (*Driver, error) {
	if scopes == nil {
		return nil, errors.New("cloud hypervisor cgroup manager is required")
	}
	if options.Binary == "" {
		options.Binary = "cloud-hypervisor"
	}
	if options.StartupTimeout == 0 {
		options.StartupTimeout = defaultStartupTimeout
	}
	if options.StartupTimeout < probeInterval {
		return nil, errors.New("cloud hypervisor startup timeout is too short")
	}
	return &Driver{paths: paths, scopes: scopes, binary: options.Binary, startupTimeout: options.StartupTimeout}, nil
}

// Preflight checks Linux/KVM and the configured binary before Starting is committed.
func (d *Driver) Preflight() error {
	if d == nil || d.scopes == nil || d.binary == "" {
		return errors.New("cloud hypervisor driver is not configured")
	}
	if err := platformPreflight(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, err)
	}
	if _, err := exec.LookPath(d.binary); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("find cloud-hypervisor: %w", err))
	}
	if err := d.paths.Ensure(); err != nil {
		return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, err)
	}
	return nil
}

// Launch starts one detached VMM directly in its cgroup, persists process
// identity, and waits until vm.info proves Running. Any error after exec kills
// the owned child and removes reconstructable runtime state.
//
//	cgroup + private dirs -> exec -> PID/start/boot identity -> process.json
//	                                                        |
//	                                  Unix API socket -> vm.info Running
func (d *Driver) Launch(ctx context.Context, plan vmm.LaunchPlan) (result vmm.Process, returnErr error) {
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
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortGrace+time.Second)
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
	vsock, _ := d.paths.Vsock(plan.SandboxID)
	args := buildArgs(plan, apiSocket, vsock)
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

	command = exec.Command(d.binary, args...) //nolint:gosec // executable is a configured fixed value; no shell is involved
	command.Stdout, command.Stderr = logFile, logFile
	configureProcess(command, scope)

	if err := command.Start(); err != nil {
		return vmm.Process{}, fmt.Errorf("exec cloud-hypervisor: %w", err)
	}
	result, err = captureProcess(command.Process.Pid, plan.SandboxID, plan.Generation, filepath.Base(d.binary), apiSocket)
	if err != nil {
		return result, fmt.Errorf("capture VMM process identity: %w", err)
	}
	if err := d.paths.WriteProcess(result); err != nil {
		return result, fmt.Errorf("persist VMM process identity: %w", err)
	}
	go func() { _ = command.Wait() }()
	if err := d.WaitReady(ctx, result); err != nil {
		return result, err
	}
	return result, nil
}

// Observe verifies process generation, boot ID, executable, unique API argument,
// socket type, and vm.info. A missing process file falls back to the owned cgroup
// to close the exec-before-identity crash window.
func (d *Driver) Observe(ctx context.Context, id types.SandboxID, generation uint64) (vmm.Observation, error) {
	if d == nil || d.scopes == nil {
		return vmm.Observation{}, errors.New("cloud hypervisor driver is not configured")
	}
	process, err := d.paths.ReadProcess(id)
	if errors.Is(err, fs.ErrNotExist) {
		process, err = d.recoverProcess(id, generation)
	}
	if err != nil {
		return vmm.Observation{}, err
	}
	if process.PID == 0 {
		return vmm.Observation{State: vmm.ProcessAbsent}, nil
	}
	alive, err := verifyProcess(process)
	if err != nil {
		return vmm.Observation{}, err
	}
	if !alive {
		return vmm.Observation{State: vmm.ProcessAbsent}, nil
	}
	if process.Generation != generation {
		return vmm.Observation{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("live VMM belongs to Starting generation %d, expected %d", process.Generation, generation))
	}
	state, err := d.queryState(ctx, process.APISocket)
	if err != nil {
		if socketUnavailable(err) {
			return vmm.Observation{State: vmm.ProcessStarting, Process: process}, nil
		}
		return vmm.Observation{}, err
	}
	if state == "Running" {
		return vmm.Observation{State: vmm.ProcessRunning, Process: process}, nil
	}
	return vmm.Observation{State: vmm.ProcessStarting, Process: process}, nil
}

// WaitReady waits for the exact process identity to expose a Running VM.
func (d *Driver) WaitReady(ctx context.Context, process vmm.Process) error {
	deadline := time.NewTimer(d.startupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		observation, err := d.Observe(ctx, process.SandboxID, process.Generation)
		if err != nil {
			return err
		}
		switch observation.State {
		case vmm.ProcessRunning:
			if observation.Process != process {
				return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("VMM process identity changed during startup"))
			}
			return nil
		case vmm.ProcessAbsent:
			return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("cloud-hypervisor exited before reaching Running; inspect vmm.log"))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("timed out waiting for Cloud Hypervisor vm.info Running"))
		case <-ticker.C:
		}
	}
}

// Abort terminates only the exact captured process generation, then removes its
// empty cgroup and runtime directory. Signal delivery uses a pidfd on Linux.
func (d *Driver) Abort(ctx context.Context, process vmm.Process) error {
	if err := process.Validate(); err != nil {
		return err
	}
	if err := terminateProcess(ctx, process, abortGrace); err != nil {
		return err
	}
	return d.Cleanup(ctx, process.SandboxID)
}

// Cleanup removes runtime state and an empty cgroup after absence is proven.
func (d *Driver) Cleanup(ctx context.Context, id types.SandboxID) error {
	if err := d.scopes.Remove(ctx, id); err != nil {
		return err
	}
	return d.paths.Clear(id)
}

// recoverProcess inspects only the sandbox's cgroup and refuses unknown members.
func (d *Driver) recoverProcess(id types.SandboxID, generation uint64) (vmm.Process, error) {
	pids, err := d.scopes.PIDs(id)
	if err != nil {
		return vmm.Process{}, err
	}
	if len(pids) == 0 {
		return vmm.Process{}, nil
	}
	apiSocket, err := d.paths.APISocket(id)
	if err != nil {
		return vmm.Process{}, err
	}
	var matches []vmm.Process
	for _, pid := range pids {
		process, match, err := identifyProcess(pid, id, generation, filepath.Base(d.binary), apiSocket)
		if err != nil {
			return vmm.Process{}, err
		}
		if match {
			matches = append(matches, process)
		}
	}
	if len(matches) != 1 || len(pids) != 1 {
		return vmm.Process{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("cgroup contains %d process(es), %d matching the owned VMM", len(pids), len(matches)))
	}
	if err := d.paths.WriteProcess(matches[0]); err != nil {
		return vmm.Process{}, err
	}
	return matches[0], nil
}

// queryState performs one bounded request over the private Unix socket.
func (d *Driver) queryState(ctx context.Context, socket string) (string, error) {
	info, err := os.Lstat(socket)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("cloud hypervisor API path is not a Unix socket"))
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: probeTimeout}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/api/v1/vm.info", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close() //nolint:errcheck // response decode error is authoritative
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxAPIResponse))
		return "", fmt.Errorf("cloud hypervisor vm.info returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		State string `json:"state"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponse+1))
	if err := decoder.Decode(&payload); err != nil {
		return "", fmt.Errorf("decode Cloud Hypervisor vm.info: %w", err)
	}
	if payload.State == "" {
		return "", errors.New("cloud hypervisor vm.info omitted state")
	}
	return payload.State, nil
}

func socketUnavailable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

func diagnosticCommand(binary string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, strconv.Quote(binary))
	for _, argument := range args {
		parts = append(parts, strconv.Quote(argument))
	}
	return strings.Join(parts, " ")
}
