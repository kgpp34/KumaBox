package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

func TestPingUsesHybridVsockHandshake(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck

	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		if line != "CONNECT 1024\n" {
			errCh <- errors.New("unexpected CONNECT line: " + line)
			return
		}
		if _, err := conn.Write([]byte("OK 1024\n")); err != nil {
			errCh <- err
			return
		}
		line, err = reader.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		if strings.TrimSpace(line) != `{"type":"ping"}` {
			errCh <- errors.New("unexpected ping line: " + line)
			return
		}
		_, err = conn.Write([]byte(`{"ok":true,"version":"test","os":"linux","hostname":"guest","capabilities":["exec","identity"]}` + "\n"))
		errCh <- err
	}()

	resp, err := Ping(context.Background(), socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Version != "test" || resp.OS != "linux" || resp.Hostname != "guest" {
		t.Fatalf("response = %+v", resp)
	}
	if !resp.Supports(CapabilityIdentity) {
		t.Fatalf("capabilities = %v, want identity", resp.Capabilities)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestExecStreamForwardsInputOutputAndExitCode(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck

	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close() //nolint:errcheck
		reader := bufio.NewReader(conn)
		line, readErr := reader.ReadString('\n')
		if readErr != nil || line != "CONNECT 1024\n" {
			serverErr <- errors.New("invalid CONNECT")
			return
		}
		if _, writeErr := conn.Write([]byte("OK 1024\n")); writeErr != nil {
			serverErr <- writeErr
			return
		}
		decoder := protocol.NewDecoder(reader)
		execFrame, frameErr := decoder.ReadFrame()
		if frameErr != nil || execFrame.Type != protocol.FrameExec || execFrame.Env["FOO"] != "bar" {
			serverErr <- fmt.Errorf("exec frame = %+v, error = %v", execFrame, frameErr)
			return
		}
		if err := protocol.WriteFrame(conn, protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameReady, ID: execFrame.ID}); err != nil {
			serverErr <- err
			return
		}
		var input bytes.Buffer
		for {
			frame, readErr := decoder.ReadFrame()
			if readErr != nil {
				serverErr <- readErr
				return
			}
			if frame.Type != protocol.FrameStdin {
				serverErr <- fmt.Errorf("unexpected frame: %+v", frame)
				return
			}
			input.Write(frame.Data)
			if frame.End {
				break
			}
		}
		if err := protocol.WriteFrame(conn, protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameStdout, ID: execFrame.ID, Stream: protocol.StreamStdout, Data: input.Bytes()}); err != nil {
			serverErr <- err
			return
		}
		if err := protocol.WriteFrame(conn, protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameStderr, ID: execFrame.ID, Stream: protocol.StreamStderr, Data: []byte("warning\n")}); err != nil {
			serverErr <- err
			return
		}
		serverErr <- protocol.WriteFrame(conn, protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameExit, ID: execFrame.ID, ExitCode: 9})
	}()

	var stdout, stderr bytes.Buffer
	code, err := ExecStream(context.Background(), socketPath, ExecRequest{
		Args: []string{"cat"},
		Env:  []string{"FOO=bar"},
	}, strings.NewReader("hello"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 9 || stdout.String() != "hello" || stderr.String() != "warning\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestPingPongResponseSupportsRejectsMissingCapability(t *testing.T) {
	t.Parallel()

	if (*PingPongResponse)(nil).Supports(CapabilityIdentity) {
		t.Fatal("nil response reported identity support")
	}
	resp := &PingPongResponse{Capabilities: []string{"exec"}}
	if resp.Supports(CapabilityIdentity) {
		t.Fatalf("capabilities = %v, unexpectedly support identity", resp.Capabilities)
	}
}

func TestExecUsesHybridVsockHandshake(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck

	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		if line != "CONNECT 1024\n" {
			errCh <- errors.New("unexpected CONNECT line: " + line)
			return
		}
		if _, err := conn.Write([]byte("OK 1024\n")); err != nil {
			errCh <- err
			return
		}
		line, err = reader.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		var req struct {
			Type string   `json:"type"`
			Args []string `json:"args"`
			Env  []string `json:"env"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			errCh <- err
			return
		}
		if req.Type != "exec" || len(req.Args) != 2 || req.Args[0] != "echo" || req.Args[1] != "ok" || len(req.Env) != 1 {
			errCh <- errors.New("unexpected exec request: " + line)
			return
		}
		_, err = conn.Write([]byte(`{"ok":true,"exitCode":0,"stdout":"b2sK"}` + "\n"))
		errCh <- err
	}()

	resp, err := Exec(context.Background(), socketPath, ExecRequest{
		Args: []string{"echo", "ok"},
		Env:  []string{"FOO=bar"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 0 || string(resp.Stdout) != "ok\n" {
		t.Fatalf("response = %+v", resp)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestPingMissingSocketReportsNotReady(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := Ping(ctx, filepath.Join(t.TempDir(), "missing.uds"))
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("error = %v, want ErrNotReady", err)
	}
	if !os.IsNotExist(errors.Unwrap(err)) && !strings.Contains(err.Error(), "dial guest agent") {
		t.Fatalf("unexpected error detail: %v", err)
	}
}

func TestConfigureIdentityCancelsStalledResponse(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck

	requestReceived := make(chan struct{})
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		reader := bufio.NewReader(conn)
		if _, readErr := reader.ReadString('\n'); readErr != nil {
			return
		}
		if _, writeErr := conn.Write([]byte("OK 1024\n")); writeErr != nil {
			return
		}
		if _, readErr := reader.ReadString('\n'); readErr != nil {
			return
		}
		close(requestReceived)
		_, _ = reader.ReadByte()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = ConfigureIdentity(ctx, socketPath, IdentityRequest{Hostname: "clone"})
	if err == nil || !errors.Is(err, ErrNotReady) {
		t.Fatalf("error = %v, want ErrNotReady", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled request ignored context for %s", elapsed)
	}
	select {
	case <-requestReceived:
	default:
		t.Fatal("identity request was not received")
	}
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kb-agent-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "v.sock")
}
