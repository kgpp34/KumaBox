package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

var _ vmm.Cloner = (*Driver)(nil)

// cloneDiskBinding maps an opaque captured device to target-owned storage.
// Neither loading nor reanchoring is allowed to open the source file itself.
type cloneDiskBinding struct {
	id       string
	source   string
	target   string
	readOnly bool
}

// cloneSetup contains only the native-state bindings needed by the common
// restore engine. Writable disk copies and image pins are already owned by core.
type cloneSetup struct {
	disks       []cloneDiskBinding
	snapshotDir string
	aliases     map[string]cloneDiskBinding
}

// ValidateClone checks MMIO device shape before core creates a new owner.
// Source files need not exist: native paths are redirected inside a private view.
func (d *Driver) ValidateClone(ctx context.Context, directory string, target types.SandboxConfig, mode types.MemoryRestoreMode) error {
	if err := validateMemoryMode(mode); err != nil {
		return err
	}
	if err := d.ValidateConfig(target); err != nil {
		return err
	}
	if err := d.ValidateSnapshot(ctx, directory); err != nil {
		return err
	}
	meta, err := readSnapshotConfig(directory)
	if err != nil {
		return err
	}
	if meta.Config.Machine.VCPUs != target.CPUs || meta.Config.Machine.MemoryMiB<<20 != target.Memory || len(meta.Config.NICs) != target.NICs {
		return invalid(errors.New("firecracker MMIO clone must keep the captured CPU, RAM and NIC count"))
	}
	if len(target.DataDisks)+1 != len(meta.Writable) {
		return invalid(errors.New("firecracker MMIO clone cannot add data disks; inherit the captured disk set"))
	}
	files := make(map[string]bool, len(meta.Writable))
	for _, disk := range meta.Writable {
		files[disk.File] = true
	}
	for _, disk := range target.DataDisks {
		if !files[types.DataDiskFile(disk.Name)] {
			return invalid(errors.New("clone data disks differ from the captured disk set"))
		}
	}
	return nil
}

// Clone restores independent writable disks and private-mapped RAM without
// stopping the source. Only this VM sees its native paths redirected. The
// source sandbox may already be deleted, or belong to a different data root.
//
//	validate -> private path view -> load paused with new TAP/vsock
//	         -> verify disk FDs -> reanchor paths -> CPU limit -> resume
func (d *Driver) Clone(ctx context.Context, plan vmm.ClonePlan) (vmm.Process, error) {
	if err := plan.Validate(); err != nil {
		return vmm.Process{}, invalid(err)
	}
	if err := validateMemoryMode(plan.MemoryMode); err != nil {
		return vmm.Process{}, err
	}
	if err := d.ValidateSnapshot(ctx, plan.SnapshotDir); err != nil {
		return vmm.Process{}, err
	}
	meta, err := readSnapshotConfig(plan.SnapshotDir)
	if err != nil {
		return vmm.Process{}, err
	}
	disks, err := cloneBindings(meta, plan)
	if err != nil {
		return vmm.Process{}, err
	}
	aliases, err := cloneAliases(disks)
	if err != nil {
		return vmm.Process{}, err
	}
	return d.restoreProcess(ctx, plan.RestorePlan, &cloneSetup{disks: disks, snapshotDir: plan.SnapshotDir, aliases: aliases})
}

func cloneBindings(meta snapshotConfig, plan vmm.ClonePlan) ([]cloneDiskBinding, error) {
	if meta.Config.Machine.VCPUs != plan.CPUs || len(meta.Config.NICs) != len(plan.Network.Interfaces) || len(plan.NewDataDisks) != 0 {
		return nil, invalid(errors.New("firecracker MMIO clone cannot change CPU, NIC or disk device count"))
	}
	if len(meta.Config.Drives) != len(plan.ImageDisks)+1+len(plan.DataDisks) {
		return nil, invalid(errors.New("clone disk shape differs from the capture"))
	}
	targets := map[string]string{"cow.raw": plan.WritableDisk}
	for _, disk := range plan.DataDisks {
		if disk.DirectIO != nil && *disk.DirectIO {
			return nil, invalid(errors.New("firecracker clone does not support directio=on"))
		}
		targets[types.DataDiskFile(disk.Serial)] = disk.Path
	}
	captured := make(map[string]string, len(meta.Writable))
	for _, disk := range meta.Writable {
		captured[disk.Source] = disk.File
	}
	bindings := make([]cloneDiskBinding, 0, len(meta.Config.Drives))
	for _, source := range meta.Config.Drives {
		target := ""
		if source.ReadOnly {
			// /vm/config may enumerate drives in map order. The stable device
			// ID, rather than response position, preserves attachment order.
			index, err := strconv.Atoi(strings.TrimPrefix(source.ID, "disk"))
			if err != nil || index < 0 || index >= len(plan.ImageDisks) || source.ID != fmt.Sprintf("disk%d", index) {
				return nil, invalid(errors.New("clone image disk identity differs from the capture"))
			}
			target = plan.ImageDisks[index].Path
		} else {
			target = targets[captured[source.Path]]
		}
		if target == "" {
			return nil, invalid(errors.New("clone writable disk mapping is incomplete"))
		}
		bindings = append(bindings, cloneDiskBinding{id: source.ID, source: source.Path, target: target, readOnly: source.ReadOnly})
	}
	return bindings, nil
}

// reanchorCloneDrives runs while paused. A later snapshot must name this
// clone's own disks, even after the private native aliases are gone.
func reanchorCloneDrives(ctx context.Context, socket string, disks []cloneDiskBinding) error {
	for _, disk := range disks {
		body := struct {
			ID   string `json:"drive_id"`
			Path string `json:"path_on_host"`
		}{disk.id, disk.target}
		if err := apiRequestWithTimeout(ctx, socket, http.MethodPatch, "/drives/"+url.PathEscape(disk.id), body, nil, snapshotTimeout); err != nil {
			return fmt.Errorf("reanchor cloned disk %q: %w", disk.id, err)
		}
	}
	return nil
}

// cloneAliases validates the entire mapping before creating any mount points.
// A native path cannot refer to two different files, including repeated layers.
func cloneAliases(disks []cloneDiskBinding) (map[string]cloneDiskBinding, error) {
	aliases := make(map[string]cloneDiskBinding, len(disks)*2)
	for _, disk := range disks {
		for _, path := range []string{disk.source, disk.target} {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
				return nil, invalid(errors.New("clone disk path must be clean and absolute"))
			}
			if old, exists := aliases[path]; exists && (old.target != disk.target || old.readOnly != disk.readOnly) {
				return nil, invalid(fmt.Errorf("clone path %s maps to conflicting devices", path))
			}
			aliases[path] = disk
		}
	}
	return aliases, nil
}
