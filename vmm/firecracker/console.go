package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/vmm"
)

const (
	// ConsoleRelayCommand is a private self-exec entrypoint using inherited FDs.
	// It is not a user-facing CLI command and does not load host configuration.
	ConsoleRelayCommand      = "__firecracker-console-relay"
	consoleReady        byte = 0
	consoleBusy         byte = 1
	consoleIOTimeout         = time.Second
)

// Console opens the serial stream only for the exact live monitor generation.
// The one-byte relay response distinguishes a busy console from an empty one.
func (d *Driver) Console(ctx context.Context, process vmm.Process) (io.ReadWriteCloser, error) {
	if err := process.Validate(); err != nil {
		return nil, err
	}
	alive, err := vmm.VerifyProcess(process)
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, errors.New("firecracker process is absent")
	}
	socket, err := d.paths.ConsoleSocket(process.SandboxID)
	if err != nil {
		return nil, err
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("connect firecracker console (restart the sandbox if its relay exited): %w", err))
	}
	ok := false
	defer func() {
		if !ok {
			_ = connection.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := connection.SetReadDeadline(time.Now().Add(consoleIOTimeout)); err != nil {
		return nil, err
	}
	var response [1]byte
	if _, err := io.ReadFull(connection, response[:]); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("console relay handshake: %w", err))
	}
	if response[0] == consoleBusy {
		return nil, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("sandbox console is already attached"))
	}
	if response[0] != consoleReady {
		return nil, errors.New("invalid console relay response")
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	alive, err = vmm.VerifyProcess(process)
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, errors.New("firecracker exited while connecting its console")
	}
	ok = true
	return connection, nil
}
