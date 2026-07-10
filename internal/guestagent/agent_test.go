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

	conn := &memoryConn{reader: strings.NewReader(`{"type":"exec"}` + "\n")}
	handleConn(conn)

	var resp helloResponse
	if err := json.Unmarshal(conn.writer.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == "" {
		t.Fatalf("response = %+v", resp)
	}
}
