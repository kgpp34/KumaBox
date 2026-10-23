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
	"sync"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/cgroup"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

const (
	// A 50 ms poll consumed a large fraction of an otherwise ready clone.
	// Both probes are bounded by startupTimeout, including when the VMM exits.
	probeInterval  = 5 * time.Millisecond
	probeTimeout   = 500 * time.Millisecond
	maxAPIResponse = 1 << 20
)

// errVMNotCreated is the vm.info response while direct boot is still creating
// the VM. The API socket can accept requests before that transition completes.
var errVMNotCreated = errors.New("cloud hypervisor VM is not created")

// scopeManager is the cgroup capability consumed by this process adapter.
type scopeManager interface {
	Prepare(context.Context, types.SandboxID, uint32, types.CPUPolicy) (*os.File, error)
	PrepareRestore(context.Context, types.SandboxID, uint32, types.CPUPolicy) (*os.File, error)
	ApplyLimit(context.Context, types.SandboxID, uint32, types.CPUPolicy) error
	PIDs(types.SandboxID) ([]int, error)
	Remove(context.Context, types.SandboxID) error
}

// Options configures the executable and bounded host lifecycle policy.
type Options = vmm.ProcessOptions

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
	// stopGrace bounds normal stop escalation after the advisory API request.
	stopGrace time.Duration
	// abortGrace bounds cleanup of a launch that never committed Running.
	abortGrace time.Duration
	// memorySupport caches successful capability probes for this driver's fixed
	// monitor binary. Failed or canceled probes must remain retryable.
	memorySupportMu    sync.Mutex
	memorySupportKnown bool
	memorySupportMode  string
}

var _ vmm.Backend = (*Driver)(nil)

// New constructs a driver without probing host capabilities.
func New(paths vmm.Paths, scopes *cgroup.Manager, options Options) (*Driver, error) {
	if scopes == nil {
		return nil, errors.New("cloud hypervisor cgroup manager is required")
	}
	resolved, err := options.Resolve("cloud-hypervisor")
	if err != nil {
		return nil, err
	}
	options = resolved
	return &Driver{
		paths: paths, scopes: scopes, binary: options.Binary,
		startupTimeout: options.StartupTimeout, stopGrace: options.StopGrace, abortGrace: options.AbortGrace,
	}, nil
}

// Type returns the durable backend identity stored with every owned sandbox.
func (*Driver) Type() types.VMMType { return types.VMMCloudHypervisor }

// ValidateConfig accepts the shared guest shape without monitor-specific restrictions.
func (*Driver) ValidateConfig(config types.SandboxConfig) error { return config.Validate() }

// NetworkQueues allocates one RX/TX pair per vCPU for multiqueue virtio-net.
func (*Driver) NetworkQueues(cpus uint32) int { return max(2, int(cpus)*2) }

// Preflight checks Linux/KVM and the configured binary before Starting is committed.
func (d *Driver) Preflight() error {
	if d == nil || d.scopes == nil || d.binary == "" {
		return errors.New("cloud hypervisor driver is not configured")
	}
	if err := vmm.PlatformPreflight(); err != nil {
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
	vsock, _ := d.paths.Vsock(plan.SandboxID)
	args := buildArgs(plan, apiSocket, vsock)
	if err := d.paths.WriteCmdline(plan.SandboxID, diagnosticCommand(d.binary, args)); err != nil {
		return vmm.Process{}, err
	}

	scope, err := d.scopes.Prepare(ctx, plan.SandboxID, plan.CPUs, plan.CPUPolicy)
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
	vmm.ConfigureProcess(command, scope)

	if err := vmm.StartProcess(command, plan.Network.Namespace); err != nil {
		return vmm.Process{}, fmt.Errorf("exec cloud-hypervisor: %w", err)
	}
	result, err = vmm.CaptureProcess(command.Process.Pid, plan.SandboxID, plan.Generation, filepath.Base(d.binary), apiSocket)
	if err != nil {
		return result, fmt.Errorf("capture VMM process identity: %w", err)
	}
	if err := d.paths.WriteProcess(result); err != nil {
		return result, fmt.Errorf("persist VMM process identity: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	if err := d.WaitReady(ctx, result); err != nil {
		return result, withProcessExit(err, exited)
	}
	return result, nil
}

// withProcessExit adds the child's exit status when readiness lost the VMM.
// WaitReady can observe /proc disappearance just before Cmd.Wait publishes its
// result, so give the reaper a short window without delaying normal startup.
func withProcessExit(readinessErr error, exited <-chan error) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case exitErr := <-exited:
		if exitErr == nil {
			return errors.Join(readinessErr, errors.New("cloud-hypervisor exited with status 0"))
		}
		return errors.Join(readinessErr, fmt.Errorf("cloud-hypervisor process: %w", exitErr))
	case <-timer.C:
		return readinessErr
	}
}

// Locate verifies process generation, boot ID, executable, and unique API
// argument without depending on VM API health. A missing process file falls
// back to the owned cgroup to close the exec-before-identity crash window.
func (d *Driver) Locate(_ context.Context, id types.SandboxID, generation uint64) (vmm.Process, bool, error) {
	return vmm.LocateProcess(d.paths, d.scopes, id, generation, d.binary)
}

// Observe combines identity-safe process location with the private vm.info API
// to distinguish startup from readiness.
func (d *Driver) Observe(ctx context.Context, id types.SandboxID, generation uint64) (vmm.Observation, error) {
	process, exists, err := d.Locate(ctx, id, generation)
	if errors.Is(err, vmm.ErrProcessIdentityPending) {
		return vmm.Observation{State: vmm.ProcessStarting, Process: process}, nil
	}
	if err != nil {
		return vmm.Observation{}, err
	}
	if !exists {
		return vmm.Observation{State: vmm.ProcessAbsent}, nil
	}
	return d.observeProcess(ctx, process)
}

// observeProcess maps a live VMM's API response to lifecycle state. A 404 is a
// normal pre-create state, while other API failures remain actionable errors.
func (d *Driver) observeProcess(ctx context.Context, process vmm.Process) (vmm.Observation, error) {
	state, err := d.queryState(ctx, process.APISocket)
	if err != nil {
		if socketUnavailable(err) || errors.Is(err, errVMNotCreated) {
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
	return waitReady(ctx, process, d.startupTimeout, d.Observe)
}

// waitReady owns readiness policy independently of process and HTTP adapters,
// making early exit, identity changes, cancellation, and timeout directly
// testable at the policy boundary.
func waitReady(
	ctx context.Context,
	process vmm.Process,
	timeout time.Duration,
	observe func(context.Context, types.SandboxID, uint64) (vmm.Observation, error),
) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		observation, err := observe(ctx, process.SandboxID, process.Generation)
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
	if err := vmm.TerminateProcess(ctx, process, d.abortGrace); err != nil {
		return err
	}
	return d.Cleanup(ctx, process.SandboxID)
}

// Stop first sends an ACPI power-button event unless forced. The guest window
// is separate from TERM/KILL grace; caller cancellation never triggers escalation.
func (d *Driver) Stop(ctx context.Context, process vmm.Process, options types.StopOptions) error {
	if err := options.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := process.Validate(); err != nil {
		return err
	}
	alive, err := vmm.VerifyProcess(process)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	if !options.Force {
		timeout := options.Timeout
		if timeout == 0 {
			timeout = types.DefaultStopTimeout
		}
		shutdownCtx, cancel := context.WithTimeout(ctx, timeout)
		err := d.snapshotAction(shutdownCtx, process.APISocket, "vm.power-button", nil, probeTimeout)
		if err == nil {
			err = vmm.WaitProcessExit(shutdownCtx, process, timeout)
		}
		cancel()
		if err == nil {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = d.requestShutdown(ctx, process.APISocket)
	return vmm.TerminateProcess(ctx, process, d.stopGrace)
}

// Console opens the direct-boot PTY reported by the exact live VMM. The caller
// owns the returned descriptor and closing it only detaches the console.
func (d *Driver) Console(ctx context.Context, process vmm.Process) (io.ReadWriteCloser, error) {
	if err := process.Validate(); err != nil {
		return nil, err
	}
	alive, err := vmm.VerifyProcess(process)
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("cloud-hypervisor process is absent"))
	}
	info, err := d.queryInfo(ctx, process.APISocket)
	if err != nil {
		return nil, err
	}
	if info.State != "Running" {
		return nil, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("cloud-hypervisor state is %q, not Running", info.State))
	}
	if info.Config.Console.Mode != "Pty" || info.Config.Console.File == "" {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("cloud-hypervisor console PTY is unavailable in mode %q", info.Config.Console.Mode))
	}
	console, err := openConsolePTY(info.Config.Console.File)
	if err != nil {
		return nil, err
	}
	alive, verifyErr := vmm.VerifyProcess(process)
	if verifyErr != nil || !alive {
		if verifyErr == nil {
			verifyErr = errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("cloud-hypervisor exited while opening its console"))
		}
		return nil, errors.Join(verifyErr, console.Close())
	}
	return console, nil
}

// Logs streams the persistent process output owned by this backend.
func (d *Driver) Logs(ctx context.Context, id types.SandboxID, options vmm.LogOptions, output io.Writer) error {
	return d.paths.Logs(ctx, id, options, output)
}

// Cleanup removes runtime state and an empty cgroup after absence is proven.
func (d *Driver) Cleanup(ctx context.Context, id types.SandboxID) error {
	if err := d.scopes.Remove(ctx, id); err != nil {
		return err
	}
	return d.paths.Clear(id)
}

// RemoveLogs releases persistent diagnostics only during sandbox removal.
func (d *Driver) RemoveLogs(ctx context.Context, id types.SandboxID) error {
	return d.paths.RemoveLogs(ctx, id)
}

// queryState performs one bounded request over the private Unix socket.
func (d *Driver) queryState(ctx context.Context, socket string) (string, error) {
	info, err := d.queryInfo(ctx, socket)
	if err != nil {
		return "", err
	}
	return info.State, nil
}

// vmInfo contains the readiness and console facts consumed from vm.info.
type vmInfo struct {
	State string `json:"state"`
	// MemoryActualSize excludes pages currently reclaimed by the balloon.
	MemoryActualSize int64 `json:"memory_actual_size"`
	Config           struct {
		Memory struct {
			Shared bool  `json:"shared"`
			Size   int64 `json:"size"`
		} `json:"memory"`
		Balloon *struct {
			Size int64 `json:"size"`
		} `json:"balloon"`
		Console struct {
			Mode string `json:"mode"`
			File string `json:"file"`
		} `json:"console"`
		Nets []struct {
			ID  string `json:"id"`
			TAP string `json:"tap"`
			MAC string `json:"mac"`
		} `json:"net"`
		Disks   []vmDiskInfo      `json:"disks"`
		Devices []vmPCIDeviceInfo `json:"devices"`
		FS      []vmFSInfo        `json:"fs"`
	} `json:"config"`
	DeviceTree map[string]json.RawMessage `json:"device_tree"`
}

type vmDiskInfo struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Serial   string `json:"serial"`
	ReadOnly bool   `json:"readonly"`
}

type vmPCIDeviceInfo struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type vmFSInfo struct {
	ID     string `json:"id"`
	Tag    string `json:"tag"`
	Socket string `json:"socket"`
}

// queryInfo performs one bounded vm.info request over the private Unix socket.
func (d *Driver) queryInfo(ctx context.Context, socket string) (vmInfo, error) {
	client, closeClient, err := unixAPIClient(socket)
	if err != nil {
		return vmInfo{}, err
	}
	defer closeClient()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/api/v1/vm.info", nil)
	if err != nil {
		return vmInfo{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return vmInfo{}, err
	}
	defer response.Body.Close() //nolint:errcheck // response decode error is authoritative
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxAPIResponse))
		if response.StatusCode == http.StatusNotFound {
			return vmInfo{}, fmt.Errorf("cloud hypervisor vm.info returned HTTP %d: %w", response.StatusCode, errVMNotCreated)
		}
		return vmInfo{}, fmt.Errorf("cloud hypervisor vm.info returned HTTP %d", response.StatusCode)
	}
	var payload vmInfo
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponse+1))
	if err := decoder.Decode(&payload); err != nil {
		return vmInfo{}, fmt.Errorf("decode Cloud Hypervisor vm.info: %w", err)
	}
	if payload.State == "" {
		return vmInfo{}, errors.New("cloud hypervisor vm.info omitted state")
	}
	return payload, nil
}

// openConsolePTY accepts only the kernel-owned /dev/pts/N shape returned by
// Cloud Hypervisor and verifies the opened descriptor is a character device.
func openConsolePTY(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	index, parseErr := strconv.Atoi(filepath.Base(clean))
	if !filepath.IsAbs(path) || filepath.Dir(clean) != "/dev/pts" || parseErr != nil || index < 0 {
		return nil, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("invalid cloud-hypervisor console PTY path %q", path))
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("stat console PTY: %w", err))
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return nil, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, fmt.Errorf("console PTY %s is not a character device", clean))
	}
	file, err := os.OpenFile(clean, os.O_RDWR, 0) //nolint:gosec // path is restricted to a validated kernel PTY leaf
	if err != nil {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("open console PTY: %w", err))
	}
	opened, err := file.Stat()
	if err != nil || opened.Mode()&os.ModeCharDevice == 0 {
		return nil, errors.Join(errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("opened console is not a character device")), file.Close())
	}
	return file, nil
}

// requestShutdown asks Cloud Hypervisor to stop its VM before process signals
// are used. Callers deliberately treat failure as advisory.
func (d *Driver) requestShutdown(ctx context.Context, socket string) error {
	client, closeClient, err := unixAPIClient(socket)
	if err != nil {
		return err
	}
	defer closeClient()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/api/v1/vm.shutdown", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close() //nolint:errcheck // status is authoritative
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxAPIResponse))
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cloud hypervisor vm.shutdown returned HTTP %d", response.StatusCode)
	}
	return nil
}

// unixAPIClient validates the private socket before constructing a bounded
// HTTP client. The close function releases idle Unix connections.
func unixAPIClient(socket string) (*http.Client, func(), error) {
	info, err := os.Lstat(socket)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, nil, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, errors.New("cloud hypervisor API path is not a Unix socket"))
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &http.Client{Transport: transport, Timeout: probeTimeout}, transport.CloseIdleConnections, nil
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
