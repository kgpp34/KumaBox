//go:build linux

package server

import (
	"bytes"
	"net"
	"testing"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

func TestHandleConnTTYExecUsesPTYAndMergesOutput(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	serverDone := make(chan struct{})
	go func() {
		handleConn(serverConn)
		_ = serverConn.Close()
		close(serverDone)
	}()

	request := protocol.Frame{
		Version: protocol.VersionV1,
		Type:    protocol.FrameExec,
		ID:      "tty-test",
		Args:    []string{"sh", "-c", "printf out; printf err >&2"},
		TTY:     true,
		Rows:    24,
		Columns: 80,
	}
	if err := protocol.WriteFrame(clientConn, request); err != nil {
		t.Fatal(err)
	}
	decoder := protocol.NewDecoder(clientConn)
	var output bytes.Buffer
	exitCode := -1
	for exitCode < 0 {
		frame, err := decoder.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case protocol.FrameStdout:
			output.Write(frame.Data)
		case protocol.FrameExit:
			exitCode = frame.ExitCode
		}
	}
	if exitCode != 0 || output.String() != "outerr" {
		t.Fatalf("exit=%d output=%q", exitCode, output.String())
	}
	<-serverDone
}
