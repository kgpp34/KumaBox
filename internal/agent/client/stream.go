package client

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

const streamChunkSize = 32 * 1024

// ExecStream runs a non-TTY command while forwarding its three standard
// streams. The returned code is the guest process exit code.
func ExecStream(ctx context.Context, socketPath string, req ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(req.Args) == 0 || req.Args[0] == "" {
		return 127, fmt.Errorf("AGENT_EXEC_INVALID: command must not be empty")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	conn, err := dialHybridVsock(ctx, socketPath, AgentPort)
	if err != nil {
		return 127, fmt.Errorf("%w: dial guest agent: %v", ErrNotReady, err)
	}
	defer conn.Close() //nolint:errcheck
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	id := fmt.Sprintf("exec-%d", time.Now().UnixNano())
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
	}); err != nil {
		return 127, fmt.Errorf("%w: write exec frame: %v", ErrNotReady, err)
	}

	inputResults := make(chan error, 1)
	go func() {
		select {
		case inputResults <- streamInput(sessionCtx, writer, id, stdin):
		case <-sessionCtx.Done():
		}
	}()

	frames, frameErrors := readFrameStream(sessionCtx, conn)
	for {
		var frame protocol.Frame
		select {
		case <-ctx.Done():
			return 127, ctx.Err()
		case inputErr := <-inputResults:
			if inputErr != nil && ctx.Err() == nil {
				return 127, fmt.Errorf("stream guest stdin: %w", inputErr)
			}
			inputResults = nil
			continue
		case readErr := <-frameErrors:
			return 127, fmt.Errorf("%w: read stream: %v", ErrNotReady, readErr)
		case frame = <-frames:
		}
		if frame.ID != id {
			return 127, fmt.Errorf("AGENT_INVALID_FRAME: unexpected exec id %q", frame.ID)
		}
		switch frame.Type {
		case protocol.FrameReady:
			continue
		case protocol.FrameStdout:
			if _, err := stdout.Write(frame.Data); err != nil {
				return 127, fmt.Errorf("write guest stdout: %w", err)
			}
		case protocol.FrameStderr:
			if _, err := stderr.Write(frame.Data); err != nil {
				return 127, fmt.Errorf("write guest stderr: %w", err)
			}
		case protocol.FrameError:
			return 127, fmt.Errorf("%s: %s", frame.Code, frame.Message)
		case protocol.FrameExit:
			return frame.ExitCode, nil
		default:
			return 127, fmt.Errorf("AGENT_INVALID_FRAME: unexpected frame %q", frame.Type)
		}
	}
}

func readFrameStream(ctx context.Context, reader io.Reader) (<-chan protocol.Frame, <-chan error) {
	frames := make(chan protocol.Frame)
	errs := make(chan error, 1)
	go func() {
		decoder := protocol.NewDecoder(reader)
		for {
			frame, err := decoder.ReadFrame()
			if err != nil {
				select {
				case errs <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	return frames, errs
}

func streamInput(ctx context.Context, writer *lockedFrameWriter, id string, stdin io.Reader) error {
	if stdin == nil {
		return writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameStdin, ID: id, Stream: protocol.StreamStdin, End: true})
	}
	buf := make([]byte, streamChunkSize)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if writeErr := writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameStdin, ID: id, Stream: protocol.StreamStdin, Data: data}); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameStdin, ID: id, Stream: protocol.StreamStdin, End: true})
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func environmentMap(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, item, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("AGENT_EXEC_INVALID: environment must be KEY=VALUE, got %q", value)
		}
		result[key] = item
	}
	return result, nil
}

type lockedFrameWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedFrameWriter) Write(frame protocol.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return protocol.WriteFrame(w.writer, frame)
}
