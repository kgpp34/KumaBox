package cloudhypervisor

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/vm"
)

const backendConsoleTimeout = 5 * time.Second

type consoleFile struct {
	*os.File
}

func (f *consoleFile) SetSize(rows, columns uint16) error {
	if rows == 0 || columns == 0 {
		return fmt.Errorf("console dimensions must be non-zero")
	}
	return setConsoleSize(f.Fd(), rows, columns)
}

// OpenConsole resolves the PTY allocated by Cloud Hypervisor for direct boot.
// The PTY path is intentionally read from vm.info instead of guessed from the
// host, because Cloud Hypervisor owns its allocation.
func (b Backend) OpenConsole(ctx context.Context, rec *vm.VMRecord) (io.ReadWriteCloser, error) {
	if rec == nil {
		return nil, fmt.Errorf("VM record is nil")
	}
	if rec.Firmware != "" {
		return nil, fmt.Errorf("VM %s uses firmware boot; console socket is not configured", rec.Name)
	}
	info, err := queryVMInfo(ctx, rec.APISocket, backendConsoleTimeout)
	if err != nil {
		return nil, fmt.Errorf("query VM console: %w", err)
	}
	path := info.Config.Console.File
	if path == "" || !isPTYConsoleMode(info.Config.Console.Mode) {
		return nil, fmt.Errorf("VM %s has no PTY console (mode=%s)", rec.Name, info.Config.Console.Mode)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat console PTY %s: %w", path, err)
	}
	if fileInfo.Mode()&os.ModeSocket != 0 {
		conn, dialErr := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if dialErr != nil {
			return nil, fmt.Errorf("connect console socket %s: %w", path, dialErr)
		}
		return conn, nil
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open console PTY %s: %w", path, err)
	}
	return &consoleFile{File: file}, nil
}

func isPTYConsoleMode(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), "pty")
}
