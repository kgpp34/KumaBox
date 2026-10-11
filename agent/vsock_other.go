//go:build !linux

package agent

import (
	"errors"
	"net"
)

// ListenVsock reports the Linux-only guest transport on development hosts.
func ListenVsock(uint32) (net.Listener, error) {
	return nil, errors.New("AF_VSOCK guest agent is supported only on Linux")
}
