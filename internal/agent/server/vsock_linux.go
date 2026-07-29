package server

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const vsockListenRetryInterval = time.Second

var (
	serveVsockAttempt = serveVsockOnce
	waitVsockRetry    = time.Sleep
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
	for attempt := 1; ; attempt++ {
		err := serveVsockAttempt(port, handler)
		if err == nil {
			return nil
		}
		auditLog.Printf("vsock listener attempt=%d failed: %v; retrying in %s", attempt, err, vsockListenRetryInterval)
		waitVsockRetry(vsockListenRetryInterval)
	}
}

func serveVsockOnce(port uint32, handler func(io.ReadWriter)) error {
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
	auditLog.Printf("vsock listener ready port=%d", port)

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
			defer func() { _ = conn.Close() }()
			handler(conn)
		}()
	}
}
