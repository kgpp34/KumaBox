// Package client submits runtime operations to the local KumaBox node service.
// It has no metadata, VMM or host-network implementation dependencies.
package client

import (
	"context"
	"encoding/json"
	"io"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/rpc"
	"github.com/kumabox/kumabox/internal/types"
)

// SandboxReporter receives request-local progress and durable state changes.
type SandboxReporter interface {
	Status(string) error
	Committed(types.Sandbox) error
}

// SnapshotReporter receives request-local snapshot events.
type SnapshotReporter interface {
	Status(string) error
	Committed(types.Snapshot) error
}

// ImageReporter receives image conversion events without controlling the server.
type ImageReporter interface {
	Layer(int, int, types.Digest) error
	Committed(types.Image) error
}

// SandboxService is a typed client for sandbox operations.
type SandboxService struct {
	connection *rpc.Client
	reporter   SandboxReporter
}

// SnapshotService is a typed client for snapshot operations.
type SnapshotService struct {
	connection *rpc.Client
	reporter   SnapshotReporter
}

// ImageService is a typed client for image operations.
type ImageService struct {
	connection *rpc.Client
}

// MaintenanceService is a typed client for explicit collection requests.
type MaintenanceService struct{ connection *rpc.Client }

// OpenSandbox prepares a client; the first operation connects to the daemon.
func OpenSandbox(_ context.Context, cfg config.Config, reporter SandboxReporter) (*SandboxService, error) {
	return &SandboxService{connection: rpc.NewClient(cfg.SocketPath()), reporter: reporter}, nil
}

// OpenSnapshots prepares an independent snapshot client and reporter.
func OpenSnapshots(_ context.Context, cfg config.Config, reporter SnapshotReporter) (*SnapshotService, error) {
	return &SnapshotService{connection: rpc.NewClient(cfg.SocketPath()), reporter: reporter}, nil
}

// OpenImages prepares an image client without opening the metadata store.
func OpenImages(_ context.Context, cfg config.Config) (*ImageService, error) {
	return &ImageService{connection: rpc.NewClient(cfg.SocketPath())}, nil
}

// OpenMaintenance prepares an explicit node collection client.
func OpenMaintenance(_ context.Context, cfg config.Config) (*MaintenanceService, error) {
	return &MaintenanceService{connection: rpc.NewClient(cfg.SocketPath())}, nil
}

func (s *SandboxService) Close() error     { return s.connection.Close() }
func (s *SnapshotService) Close() error    { return s.connection.Close() }
func (s *ImageService) Close() error       { return s.connection.Close() }
func (s *MaintenanceService) Close() error { return s.connection.Close() }

func (s *SandboxService) call(ctx context.Context, method string, params, result any, streams rpc.Streams) error {
	streams.Event = func(frame rpc.Frame) error {
		if s.reporter == nil {
			return nil
		}
		switch frame.Type {
		case "status":
			var value string
			if err := json.Unmarshal(frame.Data, &value); err != nil {
				return err
			}
			return s.reporter.Status(value)
		case "sandbox_committed":
			var value types.Sandbox
			if err := json.Unmarshal(frame.Data, &value); err != nil {
				return err
			}
			return s.reporter.Committed(value)
		}
		return nil
	}
	return s.connection.Call(ctx, "sandbox."+method, params, result, streams)
}

func (s *SnapshotService) call(ctx context.Context, method string, params, result any, streams rpc.Streams) error {
	streams.Event = func(frame rpc.Frame) error {
		if s.reporter == nil {
			return nil
		}
		switch frame.Type {
		case "status":
			var value string
			if err := json.Unmarshal(frame.Data, &value); err != nil {
				return err
			}
			return s.reporter.Status(value)
		case "snapshot_committed":
			var value types.Snapshot
			if err := json.Unmarshal(frame.Data, &value); err != nil {
				return err
			}
			return s.reporter.Committed(value)
		}
		return nil
	}
	return s.connection.Call(ctx, "snapshot."+method, params, result, streams)
}

// CollectWithPolicy preserves partial collection results when cleanup fails.
func (s *MaintenanceService) CollectWithPolicy(ctx context.Context, policy types.SnapshotEvictionPolicy) (types.GCReport, error) {
	var result types.GCReport
	err := s.connection.Call(ctx, "maintenance.collect", policy, &result, rpc.Streams{})
	return result, err
}

// Exec streams independent stdin/stdout/stderr and returns the guest exit code.
func (s *SandboxService) Exec(ctx context.Context, ref string, command types.Command, input io.Reader, out, stderr io.Writer) (int, error) {
	var result int
	err := s.call(ctx, "exec", rpc.Target[types.Command]{Reference: ref, Options: command}, &result, rpc.Streams{Input: input, Output: out, Error: stderr})
	return result, err
}

// ReadFile streams guest bytes without buffering the file in memory.
func (s *SandboxService) ReadFile(ctx context.Context, ref, path string, out io.Writer) error {
	return s.call(ctx, "file.read", rpc.FileRequest{Reference: ref, Path: path}, nil, rpc.Streams{Output: out})
}

// WriteFile closes input when cancellation interrupts an upload.
func (s *SandboxService) WriteFile(ctx context.Context, ref, path string, options types.FileWriteOptions, input io.ReadCloser) error {
	stop := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer stop()
	return s.call(ctx, "file.write", rpc.FileRequest{Reference: ref, Path: path, Options: options}, nil, rpc.Streams{Input: input})
}

// ListFiles delivers bounded entries incrementally.
func (s *SandboxService) ListFiles(ctx context.Context, ref, path string, visit func(types.FileInfo) error) error {
	return s.connection.Call(ctx, "sandbox.file.list", rpc.FileRequest{Reference: ref, Path: path}, nil, rpc.Streams{Event: func(frame rpc.Frame) error {
		if frame.Type != "file" {
			return nil
		}
		var info types.FileInfo
		if err := json.Unmarshal(frame.Data, &info); err != nil {
			return err
		}
		return visit(info)
	}})
}

// Logs follows VMM output until completion or request cancellation.
func (s *SandboxService) Logs(ctx context.Context, ref string, options types.SandboxLogOptions, out io.Writer) error {
	return s.call(ctx, "logs", rpc.Target[types.SandboxLogOptions]{Reference: ref, Options: options}, nil, rpc.Streams{Output: out})
}
