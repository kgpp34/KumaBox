package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/kumabox/kumabox/types"
)

var errMissingExit = errors.New("agent connection closed before an exit frame")

// Run executes a command over an already connected transport. Nil stdin
// closes the guest process input immediately; nil output writers discard their
// streams.
func Run(ctx context.Context, connection io.ReadWriteCloser, command types.Command, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if err := command.Validate(); err != nil {
		return 0, fmt.Errorf("validate agent command: %w", err)
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	context.AfterFunc(sessionCtx, func() { _ = connection.Close() })

	encoder := NewEncoder(connection)
	decoder := NewDecoder(connection)
	if err := encoder.Encode(Message{Type: MessageExec, Argv: command.Args, Env: command.Env}); err != nil {
		return 0, fmt.Errorf("send exec frame: %w", err)
	}

	var inputError atomic.Pointer[error]
	if stdin == nil {
		if err := encoder.Encode(Message{Type: MessageStdinClose}); err != nil {
			return 0, fmt.Errorf("close guest stdin: %w", err)
		}
	} else {
		go sendInput(stdin, encoder, &inputError, cancel)
	}

	for {
		message, err := decoder.Decode()
		if err != nil {
			if inputErr := storedInputError(&inputError); inputErr != nil {
				return 0, inputErr
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, ctxErr
			}
			if errors.Is(err, io.EOF) {
				return 0, errMissingExit
			}
			return 0, err
		}
		switch message.Type {
		case MessageStarted:
		case MessageStdout:
			if stdout != nil {
				if _, err := stdout.Write(message.Data); err != nil {
					return 0, fmt.Errorf("write command stdout: %w", err)
				}
			}
		case MessageStderr:
			if stderr != nil {
				if _, err := stderr.Write(message.Data); err != nil {
					return 0, fmt.Errorf("write command stderr: %w", err)
				}
			}
		case MessageExit:
			if inputErr := storedInputError(&inputError); inputErr != nil {
				return 0, inputErr
			}
			return message.ExitCode, nil
		case MessageError:
			return 0, fmt.Errorf("guest agent: %s", message.Message)
		default:
			// Clients ignore unknown response messages so a
			// newer agent can add optional progress or capability frames.
		}
	}
}

func sendInput(reader io.Reader, encoder *Encoder, result *atomic.Pointer[error], cancel context.CancelFunc) {
	buffer := make([]byte, streamChunkSize)
	for {
		length, err := reader.Read(buffer)
		if length > 0 {
			if encodeErr := encoder.Encode(Message{Type: MessageStdin, Data: buffer[:length]}); encodeErr != nil {
				return
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			errorCopy := err
			result.Store(&errorCopy)
			cancel()
		}
		_ = encoder.Encode(Message{Type: MessageStdinClose})
		return
	}
}

func storedInputError(result *atomic.Pointer[error]) error {
	if err := result.Load(); err != nil {
		return fmt.Errorf("read command stdin: %w", *err)
	}
	return nil
}
