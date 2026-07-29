package client

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

// ExecTTY runs a command attached to a guest PTY. PTY output is exposed as a
// single stdout stream because a terminal intentionally merges stdout/stderr.
func ExecTTY(ctx context.Context, socketPath string, req ExecRequest, stdin io.Reader, stdout io.Writer, options TTYOptions) (int, error) {
	if len(req.Args) == 0 || req.Args[0] == "" {
		return 127, fmt.Errorf("AGENT_EXEC_INVALID: command must not be empty")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	conn, err := dialHybridVsock(ctx, socketPath, AgentPort)
	if err != nil {
		return 127, fmt.Errorf("%w: dial guest agent: %v", ErrNotReady, err)
	}
	defer conn.Close() //nolint:errcheck
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	id := fmt.Sprintf("exec-tty-%d", time.Now().UnixNano())
	writer := &lockedFrameWriter{writer: conn}
	env, err := environmentMap(req.Env)
	if err != nil {
		return 127, err
	}
	if err := writer.Write(protocol.Frame{
		Version: protocol.VersionV1,
		Type:    protocol.FrameExec,
		ID:      id,
		Args:    req.Args,
		Env:     env,
		WorkDir: req.WorkDir,
		User:    req.User,
		TTY:     true,
		Rows:    options.Rows,
		Columns: options.Columns,
	}); err != nil {
		return 127, fmt.Errorf("%w: write tty exec frame: %v", ErrNotReady, err)
	}

	inputResults := make(chan error, 1)
	go func() { inputResults <- streamInput(ctx, writer, id, stdin) }()
	frames := make(chan protocol.Frame, 1)
	frameErrors := make(chan error, 1)
	go func() {
		decoder := protocol.NewDecoder(conn)
		for {
			frame, readErr := decoder.ReadFrame()
			if readErr != nil {
				frameErrors <- readErr
				return
			}
			frames <- frame
		}
	}()

	resize := options.Resize
	signals := options.Signals
	for {
		select {
		case <-ctx.Done():
			return 127, ctx.Err()
		case frame := <-frames:
			if frame.ID != id {
				return 127, fmt.Errorf("AGENT_INVALID_FRAME: unexpected exec id %q", frame.ID)
			}
			switch frame.Type {
			case protocol.FrameReady:
			case protocol.FrameStdout, protocol.FrameStderr:
				if _, err := stdout.Write(frame.Data); err != nil {
					return 127, fmt.Errorf("write guest tty output: %w", err)
				}
			case protocol.FrameError:
				return 127, fmt.Errorf("%s: %s", frame.Code, frame.Message)
			case protocol.FrameExit:
				return frame.ExitCode, nil
			default:
				return 127, fmt.Errorf("AGENT_INVALID_FRAME: unexpected frame %q", frame.Type)
			}
		case err := <-frameErrors:
			return 127, fmt.Errorf("%w: read tty stream: %v", ErrNotReady, err)
		case err := <-inputResults:
			if err != nil && ctx.Err() == nil {
				return 127, fmt.Errorf("stream tty input: %w", err)
			}
			inputResults = nil
		case size, ok := <-resize:
			if !ok {
				resize = nil
				continue
			}
			if err := writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameResize, ID: id, Rows: size.Rows, Columns: size.Columns}); err != nil {
				return 127, err
			}
		case signal, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			if err := writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameSignal, ID: id, Signal: signal}); err != nil {
				return 127, err
			}
		}
	}
}
