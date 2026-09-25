package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
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

func TestServerReportsExitWhenCommandFinishesBeforeStdin(t *testing.T) {
	client, guest := net.Pipe()
	server := &Server{logger: log.New(io.Discard, "", 0), connections: make(map[net.Conn]struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handle(t.Context(), guest)
	}()

	encoder := NewEncoder(client)
	decoder := NewDecoder(client)
	if err := encoder.Encode(Message{Type: MessageExec, Argv: []string{"sh", "-c", "exit 19"}}); err != nil {
		t.Fatal(err)
	}
	if message, err := decoder.Decode(); err != nil || message.Type != MessageStarted {
		t.Fatalf("started response = %#v, %v", message, err)
	}
	message, err := decoder.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if message.Type != MessageExit || message.ExitCode != 19 {
		t.Fatalf("exit response = %#v", message)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stdin receiver survived command exit")
	}
}

type queueListener struct {
	connections chan net.Conn
	accepted    chan struct{}
	closed      chan struct{}
	closeOnce   sync.Once
}

func newQueueListener(capacity int) *queueListener {
	return &queueListener{
		connections: make(chan net.Conn, capacity),
		accepted:    make(chan struct{}, capacity),
		closed:      make(chan struct{}),
	}
}

func (l *queueListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.connections:
		l.accepted <- struct{}{}
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *queueListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (*queueListener) Addr() net.Addr { return testAddress("agent") }

type testAddress string

func (a testAddress) Network() string { return "test" }
func (a testAddress) String() string  { return string(a) }

func TestServerShutdownClosesIdleConnectionsAndWaits(t *testing.T) {
	const connectionCount = 3
	listener := newQueueListener(connectionCount)
	server, err := NewServer(listener, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()

	clients := make([]net.Conn, 0, connectionCount)
	for range connectionCount {
		client, guest := net.Pipe()
		if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		listener.connections <- guest
	}
	for range connectionCount {
		select {
		case <-listener.accepted:
		case <-time.After(time.Second):
			t.Fatal("server did not accept every connection")
		}
	}
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not wait for idle handlers to exit")
	}
	for index, client := range clients {
		if _, err := client.Read(make([]byte, 1)); err == nil {
			t.Fatalf("connection %d remained open after shutdown", index)
		}
		_ = client.Close()
	}
}

type failingListener struct {
	err    error
	closed bool
}

func (l *failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *failingListener) Close() error {
	l.closed = true
	return nil
}
func (*failingListener) Addr() net.Addr { return testAddress("failing") }

func TestServerReturnsPermanentAcceptError(t *testing.T) {
	failure := errors.New("accept failed permanently")
	listener := &failingListener{err: failure}
	server, err := NewServer(listener, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = server.Serve(t.Context())
	if !errors.Is(err, failure) || !listener.closed {
		t.Fatalf("Serve error = %v, listener closed = %v", err, listener.closed)
	}
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
