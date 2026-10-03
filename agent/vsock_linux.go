//go:build linux

package agent

import (
	"fmt"
	"net"

	"github.com/mdlayher/vsock"
)

// ListenVsock opens the guest endpoint and accepts only the host CID. Rejecting
// guest-local peers prevents an unprivileged guest process from asking the root
// agent to execute another command.
func ListenVsock(port uint32) (net.Listener, error) {
	listener, err := vsock.Listen(port, nil)
	if err != nil {
		return nil, fmt.Errorf("listen on vsock port %d: %w", port, err)
	}
	return &hostListener{Listener: listener}, nil
}

type hostListener struct {
	net.Listener
}

func (l *hostListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		address, ok := connection.RemoteAddr().(*vsock.Addr)
		if ok && address.ContextID == vsock.Host {
			return connection, nil
		}
		_ = connection.Close()
	}
}
