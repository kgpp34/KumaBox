//go:build !linux

package server

import (
	"fmt"
	"io"
	"runtime"
)

func serveVsock(_ uint32, _ func(io.ReadWriter)) error {
	return fmt.Errorf("vsock agent is only supported on Linux guests, got %s", runtime.GOOS)
}
