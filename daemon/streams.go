package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/moby/term"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/rpc"
	"github.com/kumabox/kumabox/types"
)

type sandboxReporter struct{ session *rpc.Session }

func (r sandboxReporter) Status(value string) error { return r.session.Emit("status", value) }
func (r sandboxReporter) Committed(value types.Sandbox) error {
	return r.session.Emit("sandbox_committed", value)
}

type snapshotReporter struct{ session *rpc.Session }

func (r snapshotReporter) Status(value string) error { return r.session.Emit("status", value) }
func (r snapshotReporter) Committed(value types.Snapshot) error {
	return r.session.Emit("snapshot_committed", value)
}

type imageReporter struct{ session *rpc.Session }

func (r imageReporter) Layer(pos, total int, digest types.Digest) error {
	return r.session.Emit("layer", rpc.LayerProgress{Position: pos, Total: total, Digest: digest})
}

func (r imageReporter) Committed(value types.Image) error {
	return r.session.Emit("image_committed", value)
}

func registerStreams(mux *http.ServeMux, app *core.Application) {
	rpc.Register(mux, "sandbox.exec", func(ctx context.Context, s *rpc.Session, p rpc.Target[types.Command]) (any, error) {
		return app.Sandboxes.Exec(ctx, p.Reference, p.Options, s.Input, s.Output("stdout"), s.Output("stderr"))
	})
	rpc.Register(mux, "sandbox.logs", func(ctx context.Context, s *rpc.Session, p rpc.Target[types.SandboxLogOptions]) (any, error) {
		return nil, app.Sandboxes.Logs(ctx, p.Reference, p.Options, s.Output("stdout"))
	})
	rpc.Register(mux, "sandbox.file.read", func(ctx context.Context, s *rpc.Session, p rpc.FileRequest) (any, error) {
		return nil, app.Sandboxes.ReadFile(ctx, p.Reference, p.Path, s.Output("stdout"))
	})
	rpc.Register(mux, "sandbox.file.write", func(ctx context.Context, s *rpc.Session, p rpc.FileRequest) (any, error) {
		return nil, app.Sandboxes.WriteFile(ctx, p.Reference, p.Path, p.Options, s.Input)
	})
	rpc.Register(mux, "sandbox.file.list", func(ctx context.Context, s *rpc.Session, p rpc.FileRequest) (any, error) {
		return nil, app.Sandboxes.ListFiles(ctx, p.Reference, p.Path, func(info types.FileInfo) error { return s.Emit("file", info) })
	})
	rpc.Register(mux, "sandbox.process.logs", func(ctx context.Context, s *rpc.Session, p rpc.ProcessOutput) (any, error) {
		return app.Sandboxes.ProcessLogs(ctx, p.Reference, p.ID, p.Options, s.Output("stdout"), s.Output("stderr"))
	})
	rpc.Register(mux, "sandbox.process.attach", func(ctx context.Context, s *rpc.Session, p rpc.ProcessOutput) (any, error) {
		return app.Sandboxes.AttachProcess(ctx, p.Reference, p.ID, p.Options, s.Output("stdout"), s.Output("stderr"))
	})
	rpc.Register(mux, "snapshot.export", func(ctx context.Context, s *rpc.Session, p rpc.Target[bool]) (any, error) {
		return app.Snapshots.WithReporter(snapshotReporter{s}).Export(ctx, p.Reference, s.Output("stdout"), p.Options)
	})
	rpc.Register(mux, "snapshot.import", func(ctx context.Context, s *rpc.Session, p rpc.SnapshotImport) (any, error) {
		return app.Snapshots.WithReporter(snapshotReporter{s}).Import(ctx, s.Input, p.Name, p.Description)
	})
	rpc.Register(mux, "sandbox.forward", func(ctx context.Context, s *rpc.Session, p rpc.Target[uint16]) (any, error) {
		return nil, app.Sandboxes.ForwardPort(ctx, p.Reference, p.Options, &forwardConnection{session: s})
	})
	rpc.Register(mux, "sandbox.console", func(ctx context.Context, s *rpc.Session, p rpc.Ref) (any, error) {
		return nil, console(ctx, s, app.Sandboxes, p.Reference)
	})
}

func console(ctx context.Context, s *rpc.Session, service *core.SandboxService, ref string) error {
	connection, err := service.Console(ctx, ref)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	if err := s.Emit("ready", nil); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close(); _ = s.Input.Close() })
	defer stop()
	done := make(chan error, 1)
	go func() {
		scanner := rpc.Scanner(s.Input)
		for {
			var input rpc.ConsoleInput
			if !scanner.Scan() {
				err := scanner.Err()
				if err == nil {
					err = io.EOF
				}
				done <- err
				_ = connection.Close()
				return
			}
			if err := json.Unmarshal(scanner.Bytes(), &input); err != nil {
				done <- err
				_ = connection.Close()
				return
			}
			if len(input.Data) > 64<<10 {
				done <- errors.New("console input frame too large")
				_ = connection.Close()
				return
			}
			if file, ok := connection.(*os.File); ok && input.Rows > 0 && input.Columns > 0 {
				_ = term.SetWinsize(file.Fd(), &term.Winsize{Height: input.Rows, Width: input.Columns})
			}
			if _, err := connection.Write(input.Data); err != nil {
				done <- err
				_ = connection.Close()
				return
			}
		}
	}()
	_, err = io.Copy(s.Output("stdout"), connection)
	_ = s.Input.Close()
	inputErr := <-done
	if errors.Is(inputErr, io.EOF) {
		inputErr = nil
	}
	return errors.Join(err, inputErr)
}

// forwardConnection maps HTTP/2 directional EOF onto the guest port protocol.
// The body is the upload half; a separate event closes only the client TCP write half.
type forwardConnection struct{ session *rpc.Session }

func (c *forwardConnection) Read(p []byte) (int, error)  { return c.session.Input.Read(p) }
func (c *forwardConnection) Write(p []byte) (int, error) { return c.session.Output("stdout").Write(p) }
func (c *forwardConnection) Close() error                { return c.session.Input.Close() }
func (c *forwardConnection) CloseWrite() error           { return c.session.Emit("write_eof", nil) }

func (c *forwardConnection) LocalAddr() net.Addr { return &net.UnixAddr{Name: "node", Net: "unix"} }

func (c *forwardConnection) RemoteAddr() net.Addr               { return &net.UnixAddr{Name: "client", Net: "unix"} }
func (c *forwardConnection) SetDeadline(t time.Time) error      { return errors.ErrUnsupported }
func (c *forwardConnection) SetReadDeadline(t time.Time) error  { return errors.ErrUnsupported }
func (c *forwardConnection) SetWriteDeadline(t time.Time) error { return c.session.SetWriteDeadline(t) }
