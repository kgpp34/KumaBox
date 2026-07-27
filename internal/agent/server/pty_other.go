//go:build !linux

package server

import (
	"bufio"
	"io"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

func handleTTYExec(_ *bufio.Reader, rw io.ReadWriter, request protocol.Frame) {
	writeStreamError(rw, request.ID, protocol.ErrorCapabilityMissing, "TTY exec is only supported by the Linux guest agent")
}
