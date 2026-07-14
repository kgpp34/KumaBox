package guestagent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
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

func TestHandleConnRespondsToHello(t *testing.T) {
	t.Parallel()

	conn := &memoryConn{reader: strings.NewReader(`{"type":"hello"}` + "\n")}
	handleConn(conn)

	var resp helloResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Version != Version || resp.OS == "" || resp.Hostname == "" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestHandleConnRejectsUnsupportedRequest(t *testing.T) {
	t.Parallel()

	conn := &memoryConn{reader: strings.NewReader(`{"type":"unknown"}` + "\n")}
	handleConn(conn)

	var resp helloResponse
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
