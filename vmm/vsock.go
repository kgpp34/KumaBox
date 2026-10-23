package vmm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/kumabox/kumabox/errdefs"
)

const hybridVsockReplyLimit = 256

// DialHybridVsock opens a Firecracker-compatible Unix vsock port without
// buffering past the handshake. The adapter must verify process ownership.
func DialHybridVsock(ctx context.Context, socket string, port uint32) (io.ReadWriteCloser, error) {
	if port == 0 {
		return nil, errors.New("vsock port must be positive")
	}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("connect hybrid vsock: %w", err))
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	if _, err := fmt.Fprintf(connection, "CONNECT %d\n", port); err != nil {
		stopCancel()
		_ = connection.Close()
		return nil, fmt.Errorf("write hybrid vsock request: %w", err)
	}
	reply, err := readHybridVsockReply(connection)
	stopCancel()
	if err != nil {
		_ = connection.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("read hybrid vsock reply: %w", err)
	}
	if err := validateHybridVsockReply(reply); err != nil {
		_ = connection.Close()
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("connect guest vsock port %d: %w", port, err))
	}
	return connection, nil
}

// validateHybridVsockReply accepts the connection identifier allocated by the
// VMM. The numeric value is not an echo of the requested guest port.
func validateHybridVsockReply(reply string) error {
	fields := strings.Fields(reply)
	if len(fields) != 2 || fields[0] != "OK" {
		return fmt.Errorf("unexpected hybrid vsock reply %q", strings.TrimSpace(reply))
	}
	assignedPort, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil || assignedPort == 0 {
		return fmt.Errorf("invalid hybrid vsock connection port %q", fields[1])
	}
	return nil
}

// readHybridVsockReply deliberately avoids buffered readers, which could
// consume bytes belonging to the first agent frame after the handshake line.
func readHybridVsockReply(reader io.Reader) (string, error) {
	buffer := make([]byte, 0, 32)
	one := []byte{0}
	for {
		length, err := reader.Read(one)
		if length > 0 {
			buffer = append(buffer, one[0])
			if one[0] == '\n' {
				return string(buffer), nil
			}
			if len(buffer) >= hybridVsockReplyLimit {
				return "", fmt.Errorf("reply exceeds %d bytes", hybridVsockReplyLimit)
			}
		}
		if err != nil {
			return "", err
		}
	}
}
