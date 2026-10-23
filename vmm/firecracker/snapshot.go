package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

const (
	snapshotStateFile      = "vmstate"
	snapshotMemoryFile     = "mem"
	snapshotConfigFile     = "firecracker.json"
	snapshotTimeout        = 10 * time.Minute
	maxSnapshotConfigBytes = 1 << 20
)

var (
	_ vmm.Snapshotter      = (*Driver)(nil)
	_ vmm.RestoreValidator = (*Driver)(nil)
)

// snapshotConfig is a versioned sidecar for the opaque native device state.
// Disks retain their exact host paths: only same-owner restore is supported.
// Writable maps every writable device to its immutable capture file.
type snapshotConfig struct {
	Version   int             `json:"version"`
	SandboxID types.SandboxID `json:"sandbox_id"`
	Config    vmConfig        `json:"config"`
	Writable  []capturedDisk  `json:"writable"`
}

type capturedDisk struct {
	// Source is the resident path encoded in the native device state.
	Source string `json:"source"`
	// File is a flat, immutable COW or managed-data file inside the capture.
	File string `json:"file"`
}

type snapshotCreate struct {
	State  string `json:"snapshot_path"`
	Memory string `json:"mem_file_path"`
	Type   string `json:"snapshot_type"`
}

// Snapshot captures native RAM/device state and writable disks at one paused
// point. The guest is resumed with an independent deadline even on cancellation.
//
//	verify -> validate devices -> pause -> native state -> disks -> sidecar
//	                               \--------- resume on every exit ------/
func (d *Driver) Snapshot(ctx context.Context, plan vmm.SnapshotPlan) (returnErr error) {
	if err := plan.Validate(); err != nil {
		return invalid(err)
	}
	observation, err := d.Observe(ctx, plan.Process.SandboxID, plan.Process.Generation)
	if err != nil {
		return err
	}
	if observation.State != vmm.ProcessRunning || observation.Process != plan.Process {
		return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("sandbox VMM changed before snapshot capture"))
	}
	var config vmConfig
	if err := apiRequest(ctx, plan.Process.APISocket, http.MethodGet, "/vm/config", nil, &config); err != nil {
		return err
	}
	meta := snapshotConfig{Version: 1, SandboxID: plan.Process.SandboxID, Config: config}
	for _, file := range plan.WritableFiles {
		if filepath.Dir(file.Destination) != filepath.Clean(plan.Destination) {
			return invalid(errors.New("firecracker snapshot requires a flat capture directory"))
		}
		meta.Writable = append(meta.Writable, capturedDisk{Source: file.Source, File: filepath.Base(file.Destination)})
	}
	if err := meta.validate(); err != nil {
		return err
	}
	defer func() {
		resume, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.options.StartupTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, d.setVMState(resume, plan.Process, "Resumed"))
	}()
	if err := d.setVMState(ctx, plan.Process, "Paused"); err != nil {
		return fmt.Errorf("pause firecracker: %w", err)
	}
	request := snapshotCreate{State: filepath.Join(plan.Destination, snapshotStateFile), Memory: filepath.Join(plan.Destination, snapshotMemoryFile), Type: "Full"}
	if err := apiRequestWithTimeout(ctx, plan.Process.APISocket, http.MethodPut, "/snapshot/create", request, nil, snapshotTimeout); err != nil {
		return fmt.Errorf("capture firecracker state: %w", err)
	}
	for _, file := range plan.WritableFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := storage.CloneFile(file.Destination, file.Source); err != nil {
			return fmt.Errorf("capture writable disk: %w", err)
		}
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(plan.Destination, snapshotConfigFile), raw, 0o600) //nolint:gosec // caller-owned private capture directory
}

// setVMState verifies the exact process before changing its run state. A
// timeout is not proof that pause failed, so the caller still attempts resume.
func (d *Driver) setVMState(ctx context.Context, process vmm.Process, state string) error {
	alive, err := vmm.VerifyProcess(process)
	if err != nil {
		return err
	}
	if !alive {
		return errors.New("firecracker exited before VM state change")
	}
	return apiRequestWithTimeout(ctx, process.APISocket, http.MethodPatch, "/vm", struct {
		State string `json:"state"`
	}{state}, nil, d.options.StartupTimeout)
}

func (m snapshotConfig) validate() error {
	if m.Version != 1 {
		return errors.New("unsupported firecracker snapshot sidecar version")
	}
	if _, err := types.ParseSandboxID(m.SandboxID.String()); err != nil {
		return err
	}
	if m.Config.Machine.VCPUs == 0 || m.Config.Machine.VCPUs > 32 || m.Config.Machine.MemoryMiB <= 0 || m.Config.Machine.MemoryMiB > math.MaxInt64>>20 {
		return errors.New("snapshot has invalid machine configuration")
	}
	if !filepath.IsAbs(m.Config.Boot.Kernel) || !filepath.IsAbs(m.Config.Vsock.Path) || len(m.Config.Drives) < 2 || len(m.Writable) == 0 {
		return errors.New("snapshot has incomplete native device bindings")
	}
	writable := make(map[string]bool, len(m.Writable))
	files := make(map[string]bool, len(m.Writable))
	for _, disk := range m.Writable {
		if !filepath.IsAbs(disk.Source) || writable[disk.Source] || files[disk.File] || !capturedDiskName(disk.File) {
			return errors.New("snapshot has invalid or duplicate writable disk bindings")
		}
		writable[disk.Source], files[disk.File] = true, true
	}
	if !files["cow.raw"] {
		return errors.New("snapshot has no captured COW disk")
	}
	seen := make(map[string]bool, len(m.Config.Drives))
	for _, disk := range m.Config.Drives {
		if disk.ID == "" || seen[disk.ID] || !filepath.IsAbs(disk.Path) || disk.ReadOnly == writable[disk.Path] {
			return errors.New("native disks and captured writable files do not match")
		}
		seen[disk.ID] = true
		if !disk.ReadOnly {
			delete(writable, disk.Path)
		}
	}
	if len(writable) != 0 {
		return errors.New("capture includes writable files absent from the VMM")
	}
	return nil
}

func capturedDiskName(name string) bool {
	if name == "cow.raw" {
		return true
	}
	if !strings.HasPrefix(name, "data-") || !strings.HasSuffix(name, ".raw") {
		return false
	}
	spec := types.DataDiskSpec{Name: strings.TrimSuffix(strings.TrimPrefix(name, "data-"), ".raw"), Size: types.MinDataDiskSize, FSType: "none"}
	return spec.Validate() == nil
}

func readSnapshotConfig(directory string) (snapshotConfig, error) {
	var meta snapshotConfig
	path := filepath.Join(directory, snapshotConfigFile)
	info, err := os.Lstat(path)
	if err != nil {
		return meta, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSnapshotConfigBytes {
		return meta, errors.New("snapshot sidecar is not a bounded regular file")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // bounded regular sidecar inside the managed capture
	if err != nil {
		return meta, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&meta); err != nil {
		return meta, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return meta, errors.New("snapshot sidecar has trailing data")
	}
	return meta, meta.validate()
}

// ValidateSnapshot checks portable artifact shape without opening resident
// disks or requiring the source host's paths to exist on the importing host.
func (*Driver) ValidateSnapshot(ctx context.Context, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	meta, err := readSnapshotConfig(directory)
	if err != nil {
		return err
	}
	for _, name := range []string{snapshotStateFile, snapshotMemoryFile} {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			return fmt.Errorf("snapshot %s is not a nonempty regular file", name)
		}
		if name == snapshotMemoryFile && info.Size() != meta.Config.Machine.MemoryMiB<<20 {
			return errors.New("snapshot memory size differs from the captured machine")
		}
	}
	for _, disk := range meta.Writable {
		info, err := os.Lstat(filepath.Join(directory, disk.File))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			return fmt.Errorf("snapshot disk %s is not a nonempty regular file", disk.File)
		}
	}
	return nil
}
