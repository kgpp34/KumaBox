package core

import (
	"context"
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
