package client

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"

	"github.com/kumabox/kumabox/rpc"
	"github.com/kumabox/kumabox/types"
)

func (s *SandboxService) ProcessLogs(ctx context.Context, ref, id string, options types.ProcessOutputOptions, out, stderr io.Writer) (types.ProcessOutputResult, error) {
	var result types.ProcessOutputResult
	err := s.call(ctx, "process.logs", rpc.ProcessOutput{Reference: ref, ID: id, Options: options}, &result, rpc.Streams{Output: out, Error: stderr})
	return result, err
}

func (s *SandboxService) AttachProcess(ctx context.Context, ref, id string, options types.ProcessOutputOptions, out, stderr io.Writer) (types.ProcessOutputResult, error) {
	var result types.ProcessOutputResult
	err := s.call(ctx, "process.attach", rpc.ProcessOutput{Reference: ref, ID: id, Options: options}, &result, rpc.Streams{Output: out, Error: stderr})
	return result, err
}

func (s *SandboxService) ForwardPort(ctx context.Context, ref string, port uint16, local net.Conn) error {
	stop := context.AfterFunc(ctx, func() { _ = local.Close() })
	defer stop()
	return s.connection.Call(ctx, "sandbox.forward", rpc.Target[uint16]{Reference: ref, Options: port}, nil, rpc.Streams{Input: local, Output: local, Event: func(frame rpc.Frame) error {
		if frame.Type == "write_eof" {
			if half, ok := local.(interface{ CloseWrite() error }); ok {
				return half.CloseWrite()
			}
		}
		return nil
	}})
}

func (s *SnapshotService) Export(ctx context.Context, ref string, out io.Writer, compress bool) (types.Snapshot, error) {
	var result types.Snapshot
	err := s.call(ctx, "export", rpc.Target[bool]{Reference: ref, Options: compress}, &result, rpc.Streams{Output: out})
	return result, err
}

func (s *SnapshotService) Import(ctx context.Context, input io.Reader, name, description string) (types.Snapshot, error) {
	var result types.Snapshot
	err := s.call(ctx, "import", rpc.SnapshotImport{Name: name, Description: description}, &result, rpc.Streams{Input: input})
	return result, err
}

// Console relays terminal data and resize messages through a cancelable session.
func (s *SandboxService) Console(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	input, writer := io.Pipe()
	output, reader := io.Pipe()
	connection := &consoleConnection{reader: output, writer: writer, cancel: cancel, done: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		defer close(connection.done)
		err := s.connection.Call(ctx, "sandbox.console", rpc.Ref{Reference: ref}, nil, rpc.Streams{Input: input, Output: reader, Event: func(frame rpc.Frame) error {
			if frame.Type == "ready" {
				close(ready)
			}
			return nil
		}})
		connection.err = err
		_ = input.CloseWithError(err)
		_ = reader.CloseWithError(err)
	}()
	select {
	case <-ready:
		return connection, nil
	case <-connection.done:
		_ = connection.Close()
		return nil, connection.err
	case <-ctx.Done():
		_ = connection.Close()
		return nil, ctx.Err()
	}
}

type consoleConnection struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func (c *consoleConnection) Read(data []byte) (int, error) { return c.reader.Read(data) }
func (c *consoleConnection) send(input rpc.ConsoleInput) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return json.NewEncoder(c.writer).Encode(input)
}

func (c *consoleConnection) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		n := min(len(data), 32<<10)
		if err := c.send(rpc.ConsoleInput{Data: data[:n]}); err != nil {
			return written, err
		}
		written += n
		data = data[n:]
	}
	return written, nil
}

// Resize forwards SIGWINCH dimensions without opening a host PTY in the client.
func (c *consoleConnection) Resize(rows, columns uint16) error {
	return c.send(rpc.ConsoleInput{Rows: rows, Columns: columns})
}

func (c *consoleConnection) Close() error {
	c.cancel()
	_ = c.writer.Close()
	_ = c.reader.Close()
	<-c.done
	return nil
}
