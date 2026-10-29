package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kumabox/kumabox/internal/types"
)

// Spawn starts an agent-owned command with closed stdin. Once started, losing
// the connection does not terminate it. A lost response is ambiguous: list
// processes before retrying instead of blindly spawning a duplicate.
func Spawn(ctx context.Context, connection io.ReadWriteCloser, command types.Command) (types.ProcessInfo, error) {
	if err := command.Validate(); err != nil {
		return types.ProcessInfo{}, err
	}
	var info *types.ProcessInfo
	err := exchange(ctx, connection, Message{Type: MessageSpawn, Argv: command.Args, Env: command.Env, Cwd: command.Cwd, User: command.User}, func(message Message) error {
		if message.Type == MessageDone {
			info = message.Process
		}
		return nil
	})
	if err != nil {
		return types.ProcessInfo{}, err
	}
	if info == nil {
		return types.ProcessInfo{}, errors.New("guest agent omitted started process information")
	}
	return *info, nil
}

// Processes lists retained background records; it does not enumerate arbitrary
// guest services or foreground commands owned by other connections.
func Processes(ctx context.Context, connection io.ReadWriteCloser) ([]types.ProcessInfo, error) {
	result := make([]types.ProcessInfo, 0)
	err := exchange(ctx, connection, Message{Type: MessageProcesses}, func(message Message) error {
		if message.Type == MessageProcess && message.Process != nil {
			result = append(result, *message.Process)
		}
		return nil
	})
	return result, err
}

// SignalProcess signals an agent-owned command. An exited/evicted handle never
// falls back to an arbitrary OS PID.
func SignalProcess(ctx context.Context, connection io.ReadWriteCloser, id string, signal types.ProcessSignal) error {
	if err := types.ValidateProcessID(id); err != nil {
		return err
	}
	if err := signal.Validate(); err != nil {
		return err
	}
	return exchange(ctx, connection, Message{Type: MessageSignal, ProcessID: id, Signal: signal}, func(Message) error { return nil })
}

// ProcessLogs replays currently retained output without waiting for exit.
func ProcessLogs(ctx context.Context, connection io.ReadWriteCloser, id string, options types.ProcessOutputOptions, stdout, stderr io.Writer) (types.ProcessOutputResult, error) {
	return readProcessOutput(ctx, connection, MessageLogs, id, options, stdout, stderr)
}

// AttachProcess replays retained output and follows until exit. Disconnecting
// detaches only the observer; stdin and PTY interaction are not part of attach.
func AttachProcess(ctx context.Context, connection io.ReadWriteCloser, id string, options types.ProcessOutputOptions, stdout, stderr io.Writer) (types.ProcessOutputResult, error) {
	return readProcessOutput(ctx, connection, MessageAttach, id, options, stdout, stderr)
}

func readProcessOutput(ctx context.Context, connection io.ReadWriteCloser, kind, id string, options types.ProcessOutputOptions, stdout, stderr io.Writer) (types.ProcessOutputResult, error) {
	result := types.ProcessOutputResult{Cursor: options.After}
	if err := types.ValidateProcessID(id); err != nil {
		return result, err
	}
	err := exchange(ctx, connection, Message{Type: kind, ProcessID: id, Cursor: options.After}, func(message Message) error {
		switch message.Type {
		case MessageGap:
			result.Truncated = true
			result.Cursor = message.Cursor
		case MessageStdout, MessageStderr:
			writer := stdout
			if message.Type == MessageStderr {
				writer = stderr
			}
			if writer != nil {
				written, err := writer.Write(message.Data)
				if err != nil {
					return fmt.Errorf("write process output: %w", err)
				}
				if written != len(message.Data) {
					return io.ErrShortWrite
				}
			}
			result.Cursor = message.Cursor
		case MessageDone:
			if message.Process == nil {
				return errors.New("guest agent omitted process completion information")
			}
			result.ExitCode = message.Process.ExitCode
			if message.Process.Error != "" {
				return errors.New(message.Process.Error)
			}
		}
		return nil
	})
	return result, err
}
