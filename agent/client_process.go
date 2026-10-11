package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kumabox/kumabox/types"
)

// RemoteError preserves structured guest failures for application adapters.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string { return "guest agent: " + e.Message }

// Spawn starts an agent-owned command with closed stdin. Once started, losing
// the connection does not terminate it. A lost response is ambiguous: list
// processes before retrying instead of blindly spawning a duplicate.
func Spawn(ctx context.Context, connection io.ReadWriteCloser, command types.Command) (types.ProcessInfo, error) {
	if err := command.Validate(); err != nil {
		return types.ProcessInfo{}, err
	}
	var info *types.ProcessInfo
	err := processRequest(ctx, connection, Message{Type: MessageSpawn, Argv: command.Args, Env: command.Env, Cwd: command.Cwd, User: command.User}, func(message Message) error {
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
	err := processRequest(ctx, connection, Message{Type: MessageProcesses}, func(message Message) error {
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
	return processRequest(ctx, connection, Message{Type: MessageSignal, ProcessID: id, Signal: signal}, func(Message) error { return nil })
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
	err := processRequest(ctx, connection, Message{Type: kind, ProcessID: id, Cursor: options.After}, func(message Message) error {
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

// processRequest owns one bounded request/stream exchange. Cancellation closes
// the transport, unblocking both a slow response read and a blocked request write.
func processRequest(ctx context.Context, connection io.ReadWriteCloser, request Message, receive func(Message) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	defer connection.Close() //nolint:errcheck // one operation owns the transport
	if err := NewEncoder(connection).Encode(request); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("send agent process request: %w", err)
	}
	decoder := NewDecoder(connection)
	for {
		message, err := decoder.Decode()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read agent process response: %w", err)
		}
		if message.Type == MessageError {
			return &RemoteError{Code: message.Code, Message: message.Message}
		}
		if err := receive(message); err != nil {
			return err
		}
		if message.Type == MessageDone {
			return nil
		}
	}
}
