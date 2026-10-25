//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

type pipeListener struct {
	clients chan net.Conn
	done    chan struct{}
	once    sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.clients:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (*pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "test-console", Net: "unix"} }

type serialLog struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (l *serialLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data.Write(p)
}

func (l *serialLog) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.data.Bytes())
}

type serialRelayFixture struct {
	relay    *serialRelay
	guest    net.Conn
	listener *pipeListener
	log      *serialLog
	cancel   context.CancelFunc
}

func testSerialRelay(t *testing.T) *serialRelayFixture {
	t.Helper()
	serial, guest := net.Pipe()
	listener := &pipeListener{clients: make(chan net.Conn, 1), done: make(chan struct{})}
	log := &serialLog{}
	relay := &serialRelay{serial: serial, listener: listener, log: log}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() { finished <- relay.serve(ctx, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		_ = guest.Close()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("relay I/O workers did not exit")
		}
	})
	return &serialRelayFixture{relay: relay, guest: guest, listener: listener, log: log, cancel: cancel}
}

func dialTestConsole(t *testing.T, listener *pipeListener, want byte) net.Conn {
	t.Helper()
	server, client := net.Pipe()
	listener.clients <- server
	t.Cleanup(func() { _ = client.Close() })
	if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var response [1]byte
	if _, err := io.ReadFull(client, response[:]); err != nil || response[0] != want {
		t.Fatalf("handshake = %v %v, want %v", response, err, want)
	}
	return client
}

func waitConsoleDetached(t *testing.T, relay *serialRelay) {
	t.Helper()
	limit := time.NewTimer(3 * time.Second)
	defer limit.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		relay.mu.Lock()
		free := relay.client == nil
		relay.mu.Unlock()
		if free {
			return
		}
		select {
		case <-limit.C:
			t.Fatal("console never detached")
		case <-ticker.C:
		}
	}
}

func TestSerialRelayDrainsLogsAndReconnects(t *testing.T) {
	f := testSerialRelay(t)
	if _, err := f.guest.Write([]byte("boot without a console")); err != nil {
		t.Fatal(err)
	}
	for round := range 3 {
		client := dialTestConsole(t, f.listener, consoleReady)
		payload := []byte{0, 1, 255, byte(round)}
		written := make(chan error, 1)
		go func() { _, err := f.guest.Write(payload); written <- err }()
		result := make([]byte, len(payload))
		var output []byte
		for !bytes.HasSuffix(output, payload) && len(output) < 1024 {
			var one [1]byte
			if _, err := io.ReadFull(client, one[:]); err != nil {
				t.Fatal(err)
			}
			output = append(output, one[0])
		}
		if !bytes.HasSuffix(output, payload) {
			t.Fatalf("serial output = %v", output)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		go func() { _, err := client.Write(payload); written <- err }()
		_ = f.guest.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(f.guest, result); err != nil || !bytes.Equal(result, payload) {
			t.Fatalf("serial input: %v %v", result, err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
		waitConsoleDetached(t, f.relay)
	}
	if !bytes.Contains(f.log.bytes(), []byte("boot without a console")) {
		t.Fatal("unattached output was not logged")
	}
}

func TestSerialRelayRefusesSecondClient(t *testing.T) {
	f := testSerialRelay(t)
	first := dialTestConsole(t, f.listener, consoleReady)
	second := dialTestConsole(t, f.listener, consoleBusy)
	var one [1]byte
	if _, err := second.Read(one[:]); err != io.EOF {
		t.Fatalf("busy connection not closed: %v", err)
	}
	output := make(chan error, 1)
	go func() { _, err := f.guest.Write([]byte("x")); output <- err }()
	if _, err := io.ReadFull(first, one[:]); err != nil || one[0] != 'x' {
		t.Fatalf("first client interrupted: %v", err)
	}
	if err := <-output; err != nil {
		t.Fatal(err)
	}
}

func TestSerialRelayDropsStalledClientAndKeepsDraining(t *testing.T) {
	f := testSerialRelay(t)
	stalled := dialTestConsole(t, f.listener, consoleReady)
	// net.Pipe has no kernel send buffer: output blocks until its deadline.
	if _, err := f.guest.Write([]byte("stalled output")); err != nil {
		t.Fatal(err)
	}
	_ = f.guest.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := f.guest.Write([]byte("still draining")); err != nil {
		t.Fatalf("stalled client blocked serial draining: %v", err)
	}
	waitConsoleDetached(t, f.relay)
	_ = stalled.Close()
	_ = dialTestConsole(t, f.listener, consoleReady)
	// net.Pipe.Write acknowledges the read, before the relay appends it to
	// the log. Reconnection runs on another worker and is not a log barrier.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !bytes.Contains(f.log.bytes(), []byte("still draining")) {
		select {
		case <-deadline.C:
			t.Fatal("serial log stopped after slow client")
		case <-ticker.C:
		}
	}
}

func TestSerialRelayCancellationUnblocksPendingInput(t *testing.T) {
	f := testSerialRelay(t)
	client := dialTestConsole(t, f.listener, consoleReady)
	written := make(chan error, 1)
	go func() { _, err := client.Write(bytes.Repeat([]byte("x"), 8192)); written <- err }()
	f.cancel()
	select {
	case <-written:
	case <-time.After(3 * time.Second):
		t.Fatal("client input remained blocked")
	}
}

var _ serialStream = (*os.File)(nil)
