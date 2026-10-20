package core

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// ForwardPort relays one local TCP connection to a guest loopback service.
// It releases the sandbox lock before streaming, allowing stop and removal to
// proceed. The guest transport is bound to the exact observed VMM generation.
// After validation local is closed on every return path.
func (s *SandboxService) ForwardPort(ctx context.Context, reference string, port uint16, local net.Conn) error {
	if port == 0 || local == nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("a guest TCP port and local connection are required"))
	}
	defer local.Close() //nolint:errcheck // connection-scoped operation
	return s.withGuestAgent(ctx, reference, "forward guest port", func(connection io.ReadWriteCloser) error {
		return agent.ForwardPort(ctx, connection, port, local)
	})
}

// StatFile reads metadata for a guest entry without following its final symlink.
func (s *SandboxService) StatFile(ctx context.Context, reference, path string) (types.FileInfo, error) {
	var result types.FileInfo
	err := s.guestFileOperation(ctx, reference, path, false, "stat guest file", func(connection io.ReadWriteCloser) error {
		var err error
		result, err = agent.StatFile(ctx, connection, path)
		return err
	})
	return result, err
}

// ListFiles streams directory entries; a visitor failure stops the request.
func (s *SandboxService) ListFiles(ctx context.Context, reference, path string, visit func(types.FileInfo) error) error {
	return s.guestFileOperation(ctx, reference, path, false, "list guest directory", func(connection io.ReadWriteCloser) error {
		return agent.ListFiles(ctx, connection, path, visit)
	})
}

// ReadFile streams a guest regular file to output without buffering its contents.
func (s *SandboxService) ReadFile(ctx context.Context, reference, path string, output io.Writer) error {
	return s.guestFileOperation(ctx, reference, path, false, "read guest file", func(connection io.ReadWriteCloser) error {
		return agent.ReadFile(ctx, connection, path, output)
	})
}

// WriteFile replaces a regular guest file atomically. Once transfer starts it
// owns input; Close must unblock Read so cancellation can stop an upload.
func (s *SandboxService) WriteFile(ctx context.Context, reference, path string, options types.FileWriteOptions, input io.ReadCloser) error {
	if err := options.Validate(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return s.guestFileOperation(ctx, reference, path, true, "write guest file", func(connection io.ReadWriteCloser) error {
		return agent.WriteFile(ctx, connection, path, options, input)
	})
}

// MakeDirectory creates a private guest directory, optionally with parents.
func (s *SandboxService) MakeDirectory(ctx context.Context, reference, path string, parents bool) error {
	return s.guestFileOperation(ctx, reference, path, false, "create guest directory", func(connection io.ReadWriteCloser) error {
		return agent.MakeDirectory(ctx, connection, path, parents)
	})
}

// RemoveFile removes a guest entry; recursive removal requires explicit opt-in.
func (s *SandboxService) RemoveFile(ctx context.Context, reference, path string, recursive bool) error {
	return s.guestFileOperation(ctx, reference, path, true, "remove guest file", func(connection io.ReadWriteCloser) error {
		return agent.RemoveFile(ctx, connection, path, recursive)
	})
}

// RenameFile renames within a guest filesystem; cross-filesystem moves fail.
func (s *SandboxService) RenameFile(ctx context.Context, reference, from, to string) error {
	if err := types.ValidateGuestPath(to, true); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return s.guestFileOperation(ctx, reference, from, true, "rename guest file", func(connection io.ReadWriteCloser) error {
		return agent.RenameFile(ctx, connection, from, to)
	})
}

func (s *SandboxService) guestFileOperation(ctx context.Context, reference, path string, destructive bool, operation string, run func(io.ReadWriteCloser) error) error {
	if err := types.ValidateGuestPath(path, destructive); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return s.withGuestAgent(ctx, reference, operation, run)
}

// withGuestAgent resolves an exact live VMM generation for every call. It owns
// the transport and consistently maps remote failures at the application boundary.
func (s *SandboxService) withGuestAgent(ctx context.Context, reference, operation string, run func(io.ReadWriteCloser) error) error {
	backend, process, err := s.locateRunning(ctx, reference, operation)
	if err != nil {
		return err
	}
	connection, err := backend.DialVsock(ctx, process, agent.Port)
	if err != nil {
		return errdefs.WithContext(err, errdefs.ContextInfo{Operation: operation, Entity: reference, Phase: "connect guest agent", Action: "the guest agent may still be starting; retry shortly or inspect its service"})
	}
	defer connection.Close() //nolint:errcheck // completed read/write transport
	err = run(connection)
	if err == nil {
		return nil
	}
	var remote *agent.RemoteError
	if errors.As(err, &remote) {
		class, code := errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable
		switch remote.Code {
		case "INVALID_ARGUMENT":
			class, code = errdefs.ClassInvalid, errdefs.CodeInvalidArgument
		case "NOT_FOUND":
			class, code = errdefs.ClassNotFound, errdefs.CodeNotFound
		case "CONFLICT":
			class, code = errdefs.ClassConflict, errdefs.CodeStateConflict
		}
		err = errdefs.New(class, code, err)
	}
	return errdefs.WithContext(err, errdefs.ContextInfo{Operation: operation, Entity: reference, Phase: "guest operation", Action: "inspect the guest; ensure the image includes the current agent"})
}
