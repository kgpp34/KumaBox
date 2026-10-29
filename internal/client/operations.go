package client

import (
	"context"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/rpc"
	"github.com/kumabox/kumabox/internal/types"
)

// Create submits the sandbox create operation.
func (s *SandboxService) Create(ctx context.Context, request types.CreateSandboxRequest) (types.Sandbox, error) {
	var result types.Sandbox
	err := s.call(ctx, "create", request, &result, rpc.Streams{})
	return result, err
}

// Run submits the sandbox run operation.
func (s *SandboxService) Run(ctx context.Context, request types.CreateSandboxRequest) (types.Sandbox, error) {
	var result types.Sandbox
	err := s.call(ctx, "run", request, &result, rpc.Streams{})
	return result, err
}

// List submits the sandbox list operation.
func (s *SandboxService) List(ctx context.Context, all bool) ([]types.Sandbox, error) {
	var result []types.Sandbox
	err := s.call(ctx, "list", all, &result, rpc.Streams{})
	return result, err
}

// Inspect submits the sandbox inspect operation.
func (s *SandboxService) Inspect(ctx context.Context, ref string) (types.Sandbox, error) {
	var result types.Sandbox
	err := s.call(ctx, "inspect", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// Start submits the sandbox start operation.
func (s *SandboxService) Start(ctx context.Context, ref string) (types.Sandbox, error) {
	var result types.Sandbox
	err := s.call(ctx, "start", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// StopWithOptions submits the sandbox stop operation.
func (s *SandboxService) StopWithOptions(ctx context.Context, ref string, options types.StopOptions) (types.Sandbox, error) {
	var result types.Sandbox
	err := s.call(ctx, "stop", rpc.Target[types.StopOptions]{Reference: ref, Options: options}, &result, rpc.Streams{})
	return result, err
}

// Remove submits the sandbox remove operation.
func (s *SandboxService) Remove(ctx context.Context, ref string) (types.Sandbox, error) {
	var result types.Sandbox
	err := s.call(ctx, "remove", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// Status submits the sandbox status operation.
func (s *SandboxService) Status(ctx context.Context, refs ...string) ([]types.SandboxStatus, error) {
	var result []types.SandboxStatus
	err := s.call(ctx, "status", refs, &result, rpc.Streams{})
	return result, err
}

// Reseed submits the sandbox reseed operation.
func (s *SandboxService) Reseed(ctx context.Context, ref string, machineID bool) error {
	return s.call(ctx, "reseed", rpc.Target[bool]{Reference: ref, Options: machineID}, nil, rpc.Streams{})
}

// NetConfigure submits the sandbox net.configure operation.
func (s *SandboxService) NetConfigure(ctx context.Context, ref string) (types.Sandbox, error) {
	var result types.Sandbox
	err := s.call(ctx, "net.configure", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// NetResize submits the sandbox net.resize operation.
func (s *SandboxService) NetResize(ctx context.Context, ref string, nics int) (types.Sandbox, error) {
	var result types.Sandbox
	err := s.call(ctx, "net.resize", rpc.Target[int]{Reference: ref, Options: nics}, &result, rpc.Streams{})
	return result, err
}

// AttachDisk submits the sandbox disk.attach operation.
func (s *SandboxService) AttachDisk(ctx context.Context, ref string, disk types.ExternalDisk) (types.AttachedDevices, error) {
	var result types.AttachedDevices
	err := s.call(ctx, "disk.attach", rpc.Target[types.ExternalDisk]{Reference: ref, Options: disk}, &result, rpc.Streams{})
	return result, err
}

// DetachDisk submits the sandbox disk.detach operation.
func (s *SandboxService) DetachDisk(ctx context.Context, ref, name string) (types.AttachedDevices, error) {
	var result types.AttachedDevices
	err := s.call(ctx, "disk.detach", rpc.Named{Reference: ref, Name: name}, &result, rpc.Streams{})
	return result, err
}

// AttachFileShare submits the sandbox fs.attach operation.
func (s *SandboxService) AttachFileShare(ctx context.Context, ref string, share types.FileShare) (types.AttachedDevices, error) {
	var result types.AttachedDevices
	err := s.call(ctx, "fs.attach", rpc.Target[types.FileShare]{Reference: ref, Options: share}, &result, rpc.Streams{})
	return result, err
}

// DetachFileShare submits the sandbox fs.detach operation.
func (s *SandboxService) DetachFileShare(ctx context.Context, ref, name string) (types.AttachedDevices, error) {
	var result types.AttachedDevices
	err := s.call(ctx, "fs.detach", rpc.Named{Reference: ref, Name: name}, &result, rpc.Streams{})
	return result, err
}

// AttachPCIDevice submits the sandbox pci.attach operation.
func (s *SandboxService) AttachPCIDevice(ctx context.Context, ref, pci, id string) (types.AttachedDevices, error) {
	var result types.AttachedDevices
	err := s.call(ctx, "pci.attach", rpc.PCIRequest{Reference: ref, PCI: pci, ID: id}, &result, rpc.Streams{})
	return result, err
}

// DetachPCIDevice submits the sandbox pci.detach operation.
func (s *SandboxService) DetachPCIDevice(ctx context.Context, ref, name string) (types.AttachedDevices, error) {
	var result types.AttachedDevices
	err := s.call(ctx, "pci.detach", rpc.Named{Reference: ref, Name: name}, &result, rpc.Streams{})
	return result, err
}

// AttachedDevices submits the sandbox devices operation.
func (s *SandboxService) AttachedDevices(ctx context.Context, ref string) (types.AttachedDevices, error) {
	var result types.AttachedDevices
	err := s.call(ctx, "devices", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// Spawn submits the sandbox spawn operation.
func (s *SandboxService) Spawn(ctx context.Context, ref string, command types.Command) (types.ProcessInfo, error) {
	var result types.ProcessInfo
	err := s.call(ctx, "spawn", rpc.Target[types.Command]{Reference: ref, Options: command}, &result, rpc.Streams{})
	return result, err
}

// Processes submits the sandbox process.list operation.
func (s *SandboxService) Processes(ctx context.Context, ref string) ([]types.ProcessInfo, error) {
	var result []types.ProcessInfo
	err := s.call(ctx, "process.list", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// SignalProcess submits the sandbox process.signal operation.
func (s *SandboxService) SignalProcess(ctx context.Context, ref, id string, signal types.ProcessSignal) error {
	return s.call(ctx, "process.signal", rpc.ProcessSignal{Reference: ref, ID: id, Signal: signal}, nil, rpc.Streams{})
}

// StatFile submits the sandbox file.stat operation.
func (s *SandboxService) StatFile(ctx context.Context, ref, path string) (types.FileInfo, error) {
	var result types.FileInfo
	err := s.call(ctx, "file.stat", rpc.FileRequest{Reference: ref, Path: path}, &result, rpc.Streams{})
	return result, err
}

// MakeDirectory submits the sandbox file.mkdir operation.
func (s *SandboxService) MakeDirectory(ctx context.Context, ref, path string, parents bool) error {
	return s.call(ctx, "file.mkdir", rpc.FileRequest{Reference: ref, Path: path, Parents: parents}, nil, rpc.Streams{})
}

// RemoveFile submits the sandbox file.remove operation.
func (s *SandboxService) RemoveFile(ctx context.Context, ref, path string, recursive bool) error {
	return s.call(ctx, "file.remove", rpc.FileRequest{Reference: ref, Path: path, Recursive: recursive}, nil, rpc.Streams{})
}

// RenameFile submits the sandbox file.rename operation.
func (s *SandboxService) RenameFile(ctx context.Context, ref, from, to string) error {
	return s.call(ctx, "file.rename", rpc.FileRequest{Reference: ref, Path: from, Destination: to}, nil, rpc.Streams{})
}

// Save submits the snapshot save operation.
func (s *SnapshotService) Save(ctx context.Context, request types.SaveSnapshotRequest) (types.Snapshot, error) {
	var result types.Snapshot
	err := s.call(ctx, "save", request, &result, rpc.Streams{})
	return result, err
}

// Hibernate submits the snapshot hibernate operation.
func (s *SnapshotService) Hibernate(ctx context.Context, request types.SaveSnapshotRequest) (types.Snapshot, error) {
	var result types.Snapshot
	err := s.call(ctx, "hibernate", request, &result, rpc.Streams{})
	return result, err
}

// List submits the snapshot list operation.
func (s *SnapshotService) List(ctx context.Context) ([]types.Snapshot, error) {
	var result []types.Snapshot
	err := s.call(ctx, "list", struct{}{}, &result, rpc.Streams{})
	return result, err
}

// ListForSandbox submits the snapshot list-for-sandbox operation.
func (s *SnapshotService) ListForSandbox(ctx context.Context, ref string) ([]types.Snapshot, error) {
	var result []types.Snapshot
	err := s.call(ctx, "list-for-sandbox", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// Inspect submits the snapshot inspect operation.
func (s *SnapshotService) Inspect(ctx context.Context, ref string) (types.Snapshot, error) {
	var result types.Snapshot
	err := s.call(ctx, "inspect", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// Remove submits the snapshot remove operation.
func (s *SnapshotService) Remove(ctx context.Context, ref string) (types.Snapshot, error) {
	var result types.Snapshot
	err := s.call(ctx, "remove", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// CloneWithOptions submits the snapshot clone operation.
func (s *SnapshotService) CloneWithOptions(ctx context.Context, ref string, options types.CloneOptions) (types.Sandbox, error) {
	if options.SourceDirectory != "" {
		absolute, err := filepath.Abs(options.SourceDirectory)
		if err != nil {
			return types.Sandbox{}, err
		}
		options.SourceDirectory = absolute
	}
	var result types.Sandbox
	err := s.call(ctx, "clone", rpc.Target[types.CloneOptions]{Reference: ref, Options: options}, &result, rpc.Streams{})
	return result, err
}

// RestoreWithOptions submits the snapshot restore operation.
func (s *SnapshotService) RestoreWithOptions(ctx context.Context, ref, snapshot string, options types.RestoreOptions) (types.Sandbox, error) {
	if options.SourceDirectory != "" {
		absolute, err := filepath.Abs(options.SourceDirectory)
		if err != nil {
			return types.Sandbox{}, err
		}
		options.SourceDirectory = absolute
	}
	var result types.Sandbox
	err := s.call(ctx, "restore", rpc.RestoreRequest{Reference: ref, Snapshot: snapshot, Options: options}, &result, rpc.Streams{})
	return result, err
}

// ExportDirectory submits the snapshot export-directory operation.
func (s *SnapshotService) ExportDirectory(ctx context.Context, ref, destination string) (types.Snapshot, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return types.Snapshot{}, err
	}
	destination = absolute
	var result types.Snapshot
	err = s.call(ctx, "export-directory", rpc.Named{Reference: ref, Name: destination}, &result, rpc.Streams{})
	return result, err
}

// Clone uses the default snapshot clone policy.
func (s *SnapshotService) Clone(ctx context.Context, ref, name string) (types.Sandbox, error) {
	return s.CloneWithOptions(ctx, ref, types.CloneOptions{Name: name})
}

// Restore uses the default native restore policy.
func (s *SnapshotService) Restore(ctx context.Context, ref, snapshot string) (types.Sandbox, error) {
	return s.RestoreWithOptions(ctx, ref, snapshot, types.RestoreOptions{})
}
