package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/types"
)

func TestProtocolEncodingIsStable(t *testing.T) {
	var wire bytes.Buffer
	encoder := NewEncoder(&wire)
	if err := encoder.Encode(Message{Type: MessageExec, Argv: []string{"env"}, Env: map[string]string{"FOO": "bar"}}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(Message{Type: MessageStdout, Data: []byte("hello\n")}); err != nil {
		t.Fatal(err)
	}
	want := "{\"type\":\"exec\",\"argv\":[\"env\"],\"env\":{\"FOO\":\"bar\"}}\n" +
		"{\"type\":\"stdout\",\"data\":\"aGVsbG8K\"}\n"
	if wire.String() != want {
		t.Fatalf("wire data = %q, want %q", wire.String(), want)
	}
}

func TestEncoderAllowsOnlyOneTerminalMessage(t *testing.T) {
	encoder := NewEncoder(io.Discard)
	if err := encoder.Encode(Message{Type: MessageExit}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(Message{Type: MessageStdout, Data: []byte("late")}); !errors.Is(err, errTerminalMessageSent) {
		t.Fatalf("late message error = %v", err)
	}
}

func TestDecoderRejectsOversizedFrame(t *testing.T) {
	decoder := NewDecoder(strings.NewReader(strings.Repeat("x", maximumFrameSize+1) + "\n"))
	if _, err := decoder.Decode(); err == nil {
		t.Fatal("decoder accepted an oversized frame")
	}
}

func TestRunStreamsInputOutputAndExitStatus(t *testing.T) {
	client, guest := net.Pipe()
	server := &Server{logger: log.New(io.Discard, "", 0), connections: make(map[net.Conn]struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handle(context.Background(), guest)
	}()

	var stdout, stderr bytes.Buffer
	code, err := Run(
		t.Context(), client,
		types.Command{
			Args: []string{"sh", "-c", `printf '%s:' "$KUMABOX_TEST"; cat; printf 'warning' >&2; exit 7`},
			Env:  map[string]string{"KUMABOX_TEST": "value"},
		},
		strings.NewReader("input\n"), &stdout, &stderr,
	)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || stdout.String() != "value:input\n" || stderr.String() != "warning" {
		t.Fatalf("result: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("agent session did not close")
	}
}

func TestServerCancelsCommandWhenHostDisconnects(t *testing.T) {
	client, guest := net.Pipe()
	server := &Server{logger: log.New(io.Discard, "", 0), connections: make(map[net.Conn]struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handle(context.Background(), guest)
	}()

	encoder := NewEncoder(client)
	decoder := NewDecoder(client)
	if err := encoder.Encode(Message{Type: MessageExec, Argv: []string{"sleep", "30"}}); err != nil {
		t.Fatal(err)
	}
	message, err := decoder.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if message.Type != MessageStarted {
		t.Fatalf("first response type = %q, want %q", message.Type, MessageStarted)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("guest command survived the host disconnect")
	}
}

func TestServerRejectsUnexpectedInputFrame(t *testing.T) {
	client, guest := net.Pipe()
	server := &Server{logger: log.New(io.Discard, "", 0), connections: make(map[net.Conn]struct{})}
	go server.handle(context.Background(), guest)

	encoder := NewEncoder(client)
	decoder := NewDecoder(client)
	if err := encoder.Encode(Message{Type: MessageExec, Argv: []string{"sleep", "30"}}); err != nil {
		t.Fatal(err)
	}
	if message, err := decoder.Decode(); err != nil || message.Type != MessageStarted {
		t.Fatalf("started response = %#v, %v", message, err)
	}
	if err := encoder.Encode(Message{Type: MessageStdout}); err != nil {
		t.Fatal(err)
	}
	message, err := decoder.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if message.Type != MessageError || !strings.Contains(message.Message, "unexpected frame type") {
		t.Fatalf("protocol response = %#v", message)
	}
	_ = client.Close()
}

func TestMergeEnvironmentReplacesInheritedValues(t *testing.T) {
	got := mergeEnvironment(
		[]string{"PATH=/bin", "A=old", "B=keep"},
		map[string]string{"A": "new", "C": "added"},
	)
	want := []string{"PATH=/bin", "B=keep", "A=new", "C=added"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}
