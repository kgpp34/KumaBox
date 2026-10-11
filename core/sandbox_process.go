package core

import (
	"context"
	"errors"
	"io"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// Spawn starts a background command owned by the guest agent. The sandbox
// operation lock is released before I/O so a long-lived task cannot block stop.
func (s *SandboxService) Spawn(ctx context.Context, reference string, command types.Command) (types.ProcessInfo, error) {
	if err := command.Validate(); err != nil {
		return types.ProcessInfo{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	var result types.ProcessInfo
	err := s.withGuestAgent(ctx, reference, "spawn guest process", func(connection io.ReadWriteCloser) error {
		var err error
		result, err = agent.Spawn(ctx, connection, command)
		return err
	})
	return result, err
}

// Processes lists the guest agent's retained background commands.
func (s *SandboxService) Processes(ctx context.Context, reference string) ([]types.ProcessInfo, error) {
	var result []types.ProcessInfo
	err := s.withGuestAgent(ctx, reference, "list guest processes", func(connection io.ReadWriteCloser) error {
		var err error
		result, err = agent.Processes(ctx, connection)
		return err
	})
	return result, err
}

// SignalProcess sends one supported signal to an agent-owned process group.
func (s *SandboxService) SignalProcess(ctx context.Context, reference, id string, signal types.ProcessSignal) error {
	if err := validateProcessRequest(id, signal); err != nil {
		return err
	}
	return s.withGuestAgent(ctx, reference, "signal guest process", func(connection io.ReadWriteCloser) error {
		return agent.SignalProcess(ctx, connection, id, signal)
	})
}

// ProcessLogs replays retained command output without waiting for completion.
func (s *SandboxService) ProcessLogs(ctx context.Context, reference, id string, options types.ProcessOutputOptions, stdout, stderr io.Writer) (types.ProcessOutputResult, error) {
	return s.processOutput(ctx, reference, id, "read guest process logs", func(connection io.ReadWriteCloser) (types.ProcessOutputResult, error) {
		return agent.ProcessLogs(ctx, connection, id, options, stdout, stderr)
	})
}

// AttachProcess replays and follows output; disconnect does not kill the command.
func (s *SandboxService) AttachProcess(ctx context.Context, reference, id string, options types.ProcessOutputOptions, stdout, stderr io.Writer) (types.ProcessOutputResult, error) {
	return s.processOutput(ctx, reference, id, "attach guest process", func(connection io.ReadWriteCloser) (types.ProcessOutputResult, error) {
		return agent.AttachProcess(ctx, connection, id, options, stdout, stderr)
	})
}

func (s *SandboxService) processOutput(ctx context.Context, reference, id, operation string, run func(io.ReadWriteCloser) (types.ProcessOutputResult, error)) (types.ProcessOutputResult, error) {
	if err := types.ValidateProcessID(id); err != nil {
		return types.ProcessOutputResult{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	var result types.ProcessOutputResult
	err := s.withGuestAgent(ctx, reference, operation, func(connection io.ReadWriteCloser) error {
		var err error
		result, err = run(connection)
		return err
	})
	return result, err
}

func validateProcessRequest(id string, signal types.ProcessSignal) error {
	if err := types.ValidateProcessID(id); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if err := signal.Validate(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return nil
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
	return errdefs.WithContext(err, errdefs.ContextInfo{Operation: operation, Entity: reference, Phase: "guest process operation", Action: "inspect guest processes; ensure the image includes the current agent"})
}
