package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		if strings.TrimSpace(line) != `{"type":"hello"}` {
			errCh <- errors.New("unexpected hello line: " + line)
			return
		}
		_, err = conn.Write([]byte(`{"ok":true,"version":"test","os":"linux","hostname":"guest"}` + "\n"))
		errCh <- err
	}()

	resp, err := Ping(context.Background(), socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Version != "test" || resp.OS != "linux" || resp.Hostname != "guest" {
		t.Fatalf("response = %+v", resp)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
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
	if !os.IsNotExist(errors.Unwrap(err)) && !strings.Contains(err.Error(), "dial agent") {
		t.Fatalf("unexpected error detail: %v", err)
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
