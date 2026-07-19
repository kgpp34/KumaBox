package server

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type fdConn struct {
	file *os.File
}

func (c *fdConn) Read(p []byte) (int, error) {
	return c.file.Read(p)
}

func (c *fdConn) Write(p []byte) (int, error) {
	return c.file.Write(p)
}

func (c *fdConn) Close() error {
	return c.file.Close()
}

func serveVsock(port uint32, handler func(io.ReadWriter)) error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("create vsock socket: %w", err)
	}
	defer unix.Close(fd) //nolint:errcheck

	if err := unix.Bind(fd, &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY,
		Port: port,
	}); err != nil {
		return fmt.Errorf("bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		return fmt.Errorf("listen vsock port %d: %w", port, err)
	}

	for {
		connFD, _, err := unix.Accept(fd)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("accept vsock: %w", err)
		}
		go func() {
			conn := &fdConn{file: os.NewFile(uintptr(connFD), "vsock-agent")}
			defer conn.Close() //nolint:errcheck
			handler(conn)
		}()
	}
}
