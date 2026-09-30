package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kumabox/kumabox/errdefs"
	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

var (
	shortBDF = regexp.MustCompile(`^[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	fullBDF  = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
)

// AttachDisk hot-adds a host-owned raw file for the current VMM run. Its file
// stays outside managed roots and is never deleted with the sandbox.
func (s *SandboxService) AttachDisk(ctx context.Context, reference string, disk types.ExternalDisk) (types.AttachedDevices, error) {
	if err := types.ValidateExternalDiskName(disk.Name); err != nil {
		return types.AttachedDevices{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return s.withRunningDevice(ctx, reference, func(backend vmm.Backend, process vmm.Process, record types.Sandbox) error {
		plugger, ok := backend.(vmm.DiskHotplugger)
		if !ok {
			return unsupportedDevice(record.VMM, "disk")
		}
		path, err := resolveExternalDisk(disk.Path, s.dependencies.roots)
		if err != nil {
			return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
		}
		disk.Path = path
		disk.Queues = record.Config.CPUs
		return plugger.AddDisk(ctx, process, disk)
	})
}

// DetachDisk ejects a runtime disk by its stable serial. The backing file is
// deliberately preserved so it can be attached to another sandbox.
func (s *SandboxService) DetachDisk(ctx context.Context, reference, name string) (types.AttachedDevices, error) {
	if err := types.ValidateExternalDiskName(name); err != nil {
		return types.AttachedDevices{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return s.withRunningDevice(ctx, reference, func(backend vmm.Backend, process vmm.Process, record types.Sandbox) error {
		plugger, ok := backend.(vmm.DiskHotplugger)
		if !ok {
			return unsupportedDevice(record.VMM, "disk")
		}
		return plugger.RemoveDisk(ctx, process, name)
	})
}

// AttachPCIDevice assigns one VFIO-bound host PCI device to this VMM run.
func (s *SandboxService) AttachPCIDevice(ctx context.Context, reference, pci, id string) (types.AttachedDevices, error) {
	path, err := normalizePCIPath(pci)
	if err != nil {
		return types.AttachedDevices{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if id == "" {
		id = "pci-" + strings.NewReplacer(":", "-", ".", "-").Replace(filepath.Base(path))
	}
	if err := types.ValidatePCIDeviceID(id); err != nil {
		return types.AttachedDevices{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return s.withRunningDevice(ctx, reference, func(backend vmm.Backend, process vmm.Process, record types.Sandbox) error {
		plugger, ok := backend.(vmm.PCIHotplugger)
		if !ok {
			return unsupportedDevice(record.VMM, "PCI")
		}
		if err := checkVFIODevice(path); err != nil {
			return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, err)
		}
		return plugger.AddPCIDevice(ctx, process, types.PCIDevice{Path: path, ID: id})
	})
}

// DetachPCIDevice waits for guest PCI eject before returning the host device.
func (s *SandboxService) DetachPCIDevice(ctx context.Context, reference, id string) (types.AttachedDevices, error) {
	if err := types.ValidatePCIDeviceID(id); err != nil {
		return types.AttachedDevices{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return s.withRunningDevice(ctx, reference, func(backend vmm.Backend, process vmm.Process, record types.Sandbox) error {
		plugger, ok := backend.(vmm.PCIHotplugger)
		if !ok {
			return unsupportedDevice(record.VMM, "PCI")
		}
		return plugger.RemovePCIDevice(ctx, process, id)
	})
}

// AttachedDevices reads only the current VMM's runtime attachment set.
func (s *SandboxService) AttachedDevices(ctx context.Context, reference string) (types.AttachedDevices, error) {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.runtimes.Len() == 0 {
		return types.AttachedDevices{}, errors.New("sandbox service is not configured")
	}
	record, err := s.dependencies.catalog.Resolve(ctx, reference)
	if err != nil {
		return types.AttachedDevices{}, err
	}
	if record.State != types.SandboxStateRunning || record.Generation < 2 {
		return types.AttachedDevices{}, nil
	}
	backend, err := s.dependencies.runtimes.Backend(record.VMM)
	if err != nil {
		return types.AttachedDevices{}, err
	}
	process, found, err := backend.Locate(ctx, record.ID, record.Generation-1)
	if err != nil || !found {
		return types.AttachedDevices{}, err
	}
	return liveAttachedDevices(ctx, backend, process)
}

func (s *SandboxService) withRunningDevice(ctx context.Context, reference string, apply func(vmm.Backend, vmm.Process, types.Sandbox) error) (result types.AttachedDevices, returnErr error) {
	if s == nil || s.dependencies.catalog == nil || s.dependencies.runtimes.Len() == 0 {
		return result, errors.New("sandbox service is not configured")
	}
	record, err := s.dependencies.catalog.Resolve(ctx, reference)
	if err != nil {
		return result, err
	}
	lockPath, err := s.dependencies.paths.Lock(record.ID)
	if err != nil {
		return result, err
	}
	lock := filelock.New(lockPath)
	if err := lock.Lock(ctx); err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx))) }()
	record, err = s.dependencies.catalog.Resolve(ctx, record.ID.String())
	if err != nil {
		return result, err
	}
	if record.State != types.SandboxStateRunning || record.Generation < 2 {
		return result, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("sandbox %s is %s, not running", record.ID, record.State))
	}
	backend, err := s.dependencies.runtimes.Backend(record.VMM)
	if err != nil {
		return result, err
	}
	process, found, err := backend.Locate(ctx, record.ID, record.Generation-1)
	if err != nil {
		return result, err
	}
	if !found {
		return result, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, errors.New("running sandbox VMM is absent"))
	}
	if err := apply(backend, process, record); err != nil {
		return result, err
	}
	return liveAttachedDevices(ctx, backend, process)
}

func liveAttachedDevices(ctx context.Context, backend vmm.Backend, process vmm.Process) (types.AttachedDevices, error) {
	var result types.AttachedDevices
	if disks, ok := backend.(vmm.DiskHotplugger); ok {
		attached, err := disks.AttachedDisks(ctx, process)
		if err != nil {
			return result, err
		}
		result.Disks = attached
	}
	if pci, ok := backend.(vmm.PCIHotplugger); ok {
		attached, err := pci.AttachedPCIDevices(ctx, process)
		if err != nil {
			return result, err
		}
		result.Devices = attached
	}
	return result, nil
}

func unsupportedDevice(vmmType types.VMMType, kind string) error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("VMM %s does not support %s hotplug", vmmType, kind))
}

func resolveExternalDisk(path string, roots storage.Roots) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("external disk path %q must be absolute", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve external disk: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", errors.New("external disk must be a nonempty regular file")
	}
	for _, root := range []string{roots.Data, roots.Run, roots.Log} {
		if root == "" {
			continue
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			canonical = filepath.Clean(root)
		}
		relative, err := filepath.Rel(canonical, resolved)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("external disk %s is inside managed root %s", resolved, root)
		}
	}
	return resolved, nil
}

func normalizePCIPath(input string) (string, error) {
	const prefix = "/sys/bus/pci/devices/"
	value := strings.ToLower(strings.TrimSpace(input))
	if strings.HasPrefix(value, "/") {
		if !strings.HasPrefix(value, prefix) {
			return "", fmt.Errorf("PCI path must be under %s", prefix)
		}
		value = strings.TrimPrefix(value, prefix)
	}
	if shortBDF.MatchString(value) {
		value = "0000:" + value
	}
	if !fullBDF.MatchString(value) {
		return "", fmt.Errorf("PCI device %q must be a BDF such as 01:00.0 or 0000:01:00.0", input)
	}
	return prefix + value, nil
}

func checkVFIODevice(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("PCI device %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("PCI device %s is not a directory", path)
	}
	driver, err := filepath.EvalSymlinks(filepath.Join(path, "driver"))
	if err != nil {
		return fmt.Errorf("PCI device %s has no bound driver: %w", path, err)
	}
	if filepath.Base(driver) != "vfio-pci" {
		return fmt.Errorf("PCI device %s is bound to %s; bind it to vfio-pci first", path, filepath.Base(driver))
	}
	return nil
}
