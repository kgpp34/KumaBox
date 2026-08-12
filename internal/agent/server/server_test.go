package server

import (
	"bytes"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

type memoryConn struct {
	reader *strings.Reader
	writer bytes.Buffer
}

func (c *memoryConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *memoryConn) Write(p []byte) (int, error) {
	return c.writer.Write(p)
}

func TestHandleConnRespondsToPingPong(t *testing.T) {
	t.Parallel()

	conn := &memoryConn{reader: strings.NewReader(`{"type":"ping"}` + "\n")}
	handleConn(conn)

	var resp pingResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Version != Version || resp.OS == "" || resp.Hostname == "" {
		t.Fatalf("response = %+v", resp)
	}
	for _, capability := range []string{"ping-pong", "exec", "exec-stream", "exec-tty", "identity", "reseed"} {
		if !slices.Contains(resp.Capabilities, capability) {
			t.Fatalf("capabilities = %v, want %s", resp.Capabilities, capability)
		}
	}
	if slices.Contains(resp.Capabilities, "freeze") || slices.Contains(resp.Capabilities, "thaw") {
		t.Fatalf("capabilities = %v, freeze/thaw must not be advertised", resp.Capabilities)
	}
	if !slices.Contains(resp.Capabilities, "exec-stream") {
		t.Fatalf("capabilities = %v, want exec-stream", resp.Capabilities)
	}
}

func TestHandleConnStreamExecForwardsStreamsAndExit(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	serverDone := make(chan struct{})
	go func() {
		handleConn(serverConn)
		_ = serverConn.Close()
		close(serverDone)
	}()

	request := protocolFrameExec("sh", "-c", "cat; printf err >&2; exit 7")
	writeDone := make(chan error, 1)
	go func() {
		if err := protocol.WriteFrame(clientConn, request); err != nil {
			writeDone <- err
			return
		}
		if err := protocol.WriteFrame(clientConn, protocolFrameStdin(request.ID, []byte("hello"), false)); err != nil {
			writeDone <- err
			return
		}
		writeDone <- protocol.WriteFrame(clientConn, protocolFrameStdin(request.ID, nil, true))
	}()

	decoder := protocol.NewDecoder(clientConn)
	var stdout, stderr bytes.Buffer
	exitCode := -1
	for exitCode < 0 {
		frame, err := decoder.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case protocol.FrameStdout:
			stdout.Write(frame.Data)
		case protocol.FrameStderr:
			stderr.Write(frame.Data)
		case protocol.FrameExit:
			exitCode = frame.ExitCode
		}
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if exitCode != 7 || stdout.String() != "hello" || stderr.String() != "err" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	<-serverDone
}

func protocolFrameExec(args ...string) protocol.Frame {
	return protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameExec, ID: "stream-test", Args: args}
}

func protocolFrameStdin(id string, data []byte, end bool) protocol.Frame {
	return protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameStdin, ID: id, Stream: protocol.StreamStdin, Data: data, End: end}
}

func TestHandleConnRejectsUnsupportedRequest(t *testing.T) {
	t.Parallel()

	conn := &memoryConn{reader: strings.NewReader(`{"type":"unknown"}` + "\n")}
	handleConn(conn)

	var resp pingResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == "" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestHandleConnExecRunsCommand(t *testing.T) {
	t.Parallel()

	conn := &memoryConn{reader: strings.NewReader(`{"type":"exec","args":["sh","-c","cat; printf %s \"$FOO\""],"env":["FOO=bar"],"stdin":"aGVsbG8K"}` + "\n")}
	handleConn(conn)

	var resp execResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ExitCode != 0 || string(resp.Stdout) != "hello\nbar" {
		t.Fatalf("response = %+v stdout=%q", resp, resp.Stdout)
	}
}

func TestHandleConnExecReportsExitCode(t *testing.T) {
	t.Parallel()

	conn := &memoryConn{reader: strings.NewReader(`{"type":"exec","args":["sh","-c","echo err >&2; exit 7"]}` + "\n")}
	handleConn(conn)

	var resp execResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.ExitCode != 7 || string(resp.Stderr) != "err\n" {
		t.Fatalf("response = %+v stderr=%q", resp, resp.Stderr)
	}
}

func TestHandleConnConfiguresIdentity(t *testing.T) {
	original := configureIdentity
	defer func() { configureIdentity = original }()
	var got identityRequest
	configureIdentity = func(req identityRequest) error {
		got = req
		return nil
	}
	request := `{"type":"identity","hostname":"clone","interfaces":[{"name":"eth0","mac":"02:00:00:00:00:01","ip":"10.88.0.3","prefix":16}]}` + "\n"
	conn := &memoryConn{reader: strings.NewReader(request)}
	handleConn(conn)
	if got.Hostname != "clone" || len(got.Interfaces) != 1 || got.Interfaces[0].IP != "10.88.0.3" {
		t.Fatalf("identity request = %+v", got)
	}
	var decoded identityResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.OK {
		t.Fatalf("identity response = %+v", decoded)
	}
}

func TestHandleConnReseedsGuest(t *testing.T) {
	original := reseedGuest
	defer func() { reseedGuest = original }()
	var got reseedRequest
	reseedGuest = func(req reseedRequest) error {
		got = req
		return nil
	}
	entropy := bytes.Repeat([]byte{0x5a}, agentReseedEntropyBytes)
	raw, err := json.Marshal(reseedRequest{
		Type:                protocol.RequestReseed,
		Entropy:             entropy,
		RegenerateMachineID: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn := &memoryConn{reader: strings.NewReader(string(raw) + "\n")}
	handleConn(conn)
	if !got.RegenerateMachineID || !bytes.Equal(got.Entropy, make([]byte, agentReseedEntropyBytes)) {
		t.Fatalf("reseed request was not handled and cleared: %+v", got)
	}
	var resp reseedResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("reseed response = %+v", resp)
	}
}

func TestHandleConnRejectsInvalidReseedEntropy(t *testing.T) {
	t.Parallel()

	conn := &memoryConn{reader: strings.NewReader(`{"type":"reseed","entropy":"AQI="}` + "\n")}
	handleConn(conn)
	var resp reseedResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || !strings.Contains(resp.Error, "32 bytes") {
		t.Fatalf("reseed response = %+v", resp)
	}
}
