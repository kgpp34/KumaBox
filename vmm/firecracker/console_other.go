//go:build !linux

package firecracker

import (
	"context"
	"errors"
	"os"

	"github.com/kumabox/kumabox/vmm"
)

type consoleLaunch struct{ slave *os.File }

func prepareConsole(string) (*consoleLaunch, error) {
	return nil, errors.New("firecracker console requires Linux")
}
func (*consoleLaunch) close() {}
func (*Driver) startConsoleRelay(context.Context, vmm.Process, *consoleLaunch, *os.File) error {
	return errors.New("firecracker console requires Linux")
}

// RunConsoleRelay is the Linux-only inherited-FD helper entrypoint.
func RunConsoleRelay(context.Context) error { return errors.New("firecracker console requires Linux") }
