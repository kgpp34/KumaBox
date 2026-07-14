// Package agent implements the host-side KumaBox guest-agent client.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	AgentPort           uint32 = 1024
	hybridVsockReplyMax        = 256
	DefaultPingTimeout         = 60 * time.Second
)

var ErrNotReady = errors.New("AGENT_NOT_READY")

type HelloResponse struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version,omitempty"`
	OS       string `json:"os,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Error    string `json:"error,omitempty"`
}

type ExecRequest struct {
	Args    []string `json:"args"`
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	Stdin   []byte   `json:"stdin,omitempty"`
}

type ExecResponse struct {
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exitCode"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

func Ping(ctx context.Context, socketPath string) (*HelloResponse, error) {
	var lastErr error
	for {
		resp, err := pingOnce(ctx, socketPath)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", ErrNotReady, lastErr)
		case <-time.After(time.Second):
		}
	}
}

func pingOnce(ctx context.Context, socketPath string) (*HelloResponse, error) {
	var resp HelloResponse
	if err := roundTrip(ctx, socketPath, map[string]any{"type": "hello"}, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "agent returned not ok"
		}
		return &resp, fmt.Errorf("%w: %s", ErrNotReady, resp.Error)
	}
	return &resp, nil
}

func Exec(ctx context.Context, socketPath string, req ExecRequest) (*ExecResponse, error) {
	if len(req.Args) == 0 || req.Args[0] == "" {
		return nil, fmt.Errorf("AGENT_EXEC_INVALID: command must not be empty")
	}
	wireReq := struct {
		Type string `json:"type"`
		ExecRequest
	}{
		Type:        "exec",
		ExecRequest: req,
	}
	var resp ExecResponse
	if err := roundTrip(ctx, socketPath, wireReq, &resp); err != nil {
		return nil, err
	}
	if !resp.OK && resp.Error == "" {
		resp.Error = "agent exec returned not ok"
	}
	return &resp, nil
}

func roundTrip(ctx context.Context, socketPath string, req any, resp any) error {
	conn, err := dialHybridVsock(ctx, socketPath, AgentPort)
	if err != nil {
		return fmt.Errorf("%w: dial agent: %v", ErrNotReady, err)
	}
	defer conn.Close() //nolint:errcheck

	raw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("AGENT_REQUEST_INVALID: %w", err)
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("%w: write request: %v", ErrNotReady, err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrNotReady, err)
	}
	if err := json.Unmarshal(line, resp); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrNotReady, err)
	}
	return nil
}

func dialHybridVsock(ctx context.Context, socketPath string, port uint32) (io.ReadWriteCloser, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}
	reply, err := readHybridVsockReply(conn)
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("read CONNECT reply: %w", err)
	}
	if !strings.HasPrefix(reply, "OK ") {
		_ = conn.Close()
		return nil, fmt.Errorf("hybrid vsock CONNECT %d: %s", port, strings.TrimSpace(reply))
	}
	return conn, nil
}

func readHybridVsockReply(r io.Reader) (string, error) {
	buf := make([]byte, 0, 32)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n > 0 {
			buf = append(buf, one[0])
			if one[0] == '\n' {
				return string(buf), nil
			}
			if len(buf) >= hybridVsockReplyMax {
				return "", fmt.Errorf("reply line exceeds %d bytes", hybridVsockReplyMax)
			}
		}
		if err != nil {
			return "", err
		}
	}
}
