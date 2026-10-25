package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

var _ vmm.Restorer = (*Driver)(nil)

// snapshotLoad keeps the guest paused until target host policy is installed.
// The File backend uses MAP_PRIVATE and faults pages in on their first access.
type snapshotLoad struct {
	State  string            `json:"snapshot_path"`
	Memory snapshotMemory    `json:"mem_backend"`
	Resume bool              `json:"resume_vm"`
	NICs   []networkOverride `json:"network_overrides,omitempty"`
	Vsock  *vsockOverride    `json:"vsock_override,omitempty"`
}

type networkOverride struct {
	ID  string `json:"iface_id"`
	TAP string `json:"host_dev_name"`
}

type vsockOverride struct {
	Path string `json:"uds_path"`
}

type snapshotMemory struct {
	Path string `json:"backend_path"`
	Type string `json:"backend_type"`
}

// ValidateRestore refuses unsupported memory policy and relocated resident
// paths before core stops the current VM. Import uses ValidateSnapshot instead.
func (d *Driver) ValidateRestore(ctx context.Context, directory string, mode types.MemoryRestoreMode) error {
	if err := validateMemoryMode(mode); err != nil {
		return err
	}
	if err := d.ValidateSnapshot(ctx, directory); err != nil {
		return err
	}
	meta, err := readSnapshotConfig(directory)
	if err != nil {
		return err
	}
	if err := d.validateOwner(meta); err != nil {
		return err
	}
	for _, disk := range meta.Config.Drives {
		info, err := os.Lstat(disk.Path)
		if err != nil {
			return fmt.Errorf("inspect resident disk %q: %w", disk.ID, err)
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			return fmt.Errorf("resident disk %q is not a nonempty regular file", disk.ID)
		}
	}
	return nil
}

func validateMemoryMode(mode types.MemoryRestoreMode) error {
	if err := mode.Validate(); err != nil {
		return invalid(err)
	}
	if mode == types.MemoryRestoreCopy {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("firecracker restores memory through private mmap; use --restore-mode auto, mmap or ondemand"))
	}
	return nil
}

func (d *Driver) validateOwner(meta snapshotConfig) error {
	vsock, err := d.paths.Vsock(meta.SandboxID)
	if err != nil {
		return err
	}
	kernel, err := filepath.Rel(d.paths.KernelCache(), meta.Config.Boot.Kernel)
	if err != nil {
		return err
	}
	localData := kernel != "." && kernel != ".." && !strings.HasPrefix(kernel, ".."+string(filepath.Separator))
	if meta.Owner != nil {
		localData = meta.Owner.Data == d.paths.Roots().Data && meta.Owner.Run == d.paths.Roots().Run
	}
	if meta.Config.Vsock.Path != vsock || !localData {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("firecracker snapshot belongs to another data or runtime root; relocated restore requires clone support"))
	}
	return nil
}

// Restore launches an unconfigured monitor, loads the original owner's native
// state, installs CPU policy, and resumes. Every failed launch is terminated
// before runtime files or its console relay can be removed.
//
//	preflight -> API-only process + relay -> load paused -> CPU limit -> resume
//	                                      \--- failure: abort ---/
func (d *Driver) Restore(ctx context.Context, plan vmm.RestorePlan) (result vmm.Process, returnErr error) {
	if err := plan.Validate(); err != nil {
		return result, invalid(err)
	}
	if err := d.ValidateRestore(ctx, plan.SnapshotDir, plan.MemoryMode); err != nil {
		return result, err
	}
	meta, err := readSnapshotConfig(plan.SnapshotDir)
	if err != nil {
		return result, err
	}
	if err := validateRestoreBindings(meta, plan); err != nil {
		return result, err
	}
	return d.restoreProcess(ctx, plan, nil)
}

// restoreProcess shares process, console, CPU and failure ownership. Clone
// supplies only its mount/device bindings; ordinary restore keeps native paths.
func (d *Driver) restoreProcess(ctx context.Context, plan vmm.RestorePlan, clone *cloneSetup) (result vmm.Process, returnErr error) {
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
	scope, err := d.scopes.PrepareRestore(ctx, plan.SandboxID, plan.CPUs, plan.CPUPolicy)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, scope.Close()) }()
	api, _ := d.paths.APISocket(plan.SandboxID)
	launch := processLaunch{
		sandboxID: plan.SandboxID, generation: plan.Generation, scope: scope,
		namespace: plan.Network.Namespace,
		args:      []string{"--api-sock", api, "--id", plan.SandboxID.String(), "--level", "Warning"},
	}
	if clone == nil {
		result, err = d.launchProcess(ctx, launch)
	} else {
		result, err = d.launchCloneProcess(ctx, launch, clone)
	}
	if err != nil {
		return result, err
	}
	if err := d.waitRestoreAPI(ctx, result); err != nil {
		return result, err
	}
	request := snapshotLoad{
		State:  filepath.Join(plan.SnapshotDir, snapshotStateFile),
		Memory: snapshotMemory{Path: filepath.Join(plan.SnapshotDir, snapshotMemoryFile), Type: "File"},
	}
	if clone != nil {
		vsockPath, _ := d.paths.Vsock(plan.SandboxID)
		request.Vsock = &vsockOverride{Path: vsockPath}
		for _, nic := range plan.Network.Interfaces {
			request.NICs = append(request.NICs, networkOverride{ID: fmt.Sprintf("eth%d", nic.Index), TAP: nic.TAP})
		}
	}
	if err := apiRequestWithTimeout(ctx, api, http.MethodPut, "/snapshot/load", request, nil, snapshotTimeout); err != nil {
		return result, fmt.Errorf("load firecracker snapshot: %w", err)
	}
	if clone != nil {
		if err := verifyCloneDrives(result.PID, clone.disks); err != nil {
			return result, err
		}
		if err := reanchorCloneDrives(ctx, result.APISocket, clone.disks); err != nil {
			return result, err
		}
	}
	if err := d.scopes.ApplyLimit(ctx, plan.SandboxID, plan.CPUs, plan.CPUPolicy); err != nil {
		return result, fmt.Errorf("apply restored CPU limit: %w", err)
	}
	if err := d.setVMState(ctx, result, "Resumed"); err != nil {
		return result, fmt.Errorf("resume firecracker snapshot: %w", err)
	}
	return result, d.WaitReady(ctx, result)
}

// validateRestoreBindings disallows cross-owner reuse of opaque native paths.
// Stable TAP identities are retained by same-sandbox CNI reconciliation.
func validateRestoreBindings(meta snapshotConfig, plan vmm.RestorePlan) error {
	if meta.SandboxID != plan.SandboxID || meta.Config.Machine.VCPUs != plan.CPUs {
		return invalid(errors.New("firecracker restore requires the source sandbox identity and CPU shape"))
	}
	if len(meta.Config.NICs) != len(plan.Network.Interfaces) {
		return invalid(errors.New("firecracker restore network shape differs from the snapshot"))
	}
	native := make(map[string]networkInterface, len(meta.Config.NICs))
	for _, nic := range meta.Config.NICs {
		native[nic.ID] = nic
	}
	for _, nic := range plan.Network.Interfaces {
		source := native[fmt.Sprintf("eth%d", nic.Index)]
		// A guest may set its effective MAC after cloning. Native virtio config
		// retains the boot MAC while guest RAM carries the current link address.
		if source.ID != fmt.Sprintf("eth%d", nic.Index) || source.TAP != nic.TAP {
			return invalid(errors.New("firecracker restore requires the captured TAP bindings"))
		}
	}
	return nil
}

func (d *Driver) waitRestoreAPI(ctx context.Context, process vmm.Process) error {
	ctx, cancel := context.WithTimeout(ctx, d.options.StartupTimeout)
	defer cancel()
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		alive, err := vmm.VerifyProcess(process)
		if err != nil {
			return err
		}
		if !alive {
			return errors.New("firecracker restore process exited before its API became ready")
		}
		var info struct {
			State string `json:"state"`
		}
		err = apiRequest(ctx, process.APISocket, http.MethodGet, "/", nil, &info)
		if err == nil {
			if info.State != "Not started" {
				return fmt.Errorf("restore process is already configured: %q", info.State)
			}
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
