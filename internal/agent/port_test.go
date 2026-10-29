package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func portTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	for _, connection := range []*net.TCPConn{client, server} {
		if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return client, server
}

func portNumber(t *testing.T, address net.Addr) uint16 {
	t.Helper()
	_, port, err := net.SplitHostPort(address.String())
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(number)
}

func awaitPort(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("port relay did not stop")
		return nil
	}
}

func TestForwardPortHTTPAndConcurrentStreams(t *testing.T) {
	payload := bytes.Repeat([]byte{0, 1, 255, '\n'}, 256*1024)
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = w.Write(body)
	}))
	defer guest.Close()
	server := processServer(t)
	port := portNumber(t, guest.Listener.Addr())
	var clients sync.WaitGroup
	for range 8 {
		client, local := portTCPPair(t)
		transport := processConnection(t, server)
		clients.Add(1)
		go func() {
			defer clients.Done()
			forwarded := make(chan error, 1)
			go func() { forwarded <- ForwardPort(t.Context(), transport, port, local) }()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://guest/echo", bytes.NewReader(payload))
			if err != nil {
				t.Error(err)
				return
			}
			request.Close = true
			if err := request.Write(client); err != nil {
				t.Error(err)
				return
			}
			response, err := http.ReadResponse(bufio.NewReader(client), request)
			if err != nil {
				t.Error(err)
				return
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			_ = client.CloseWrite()
			if err != nil || !bytes.Equal(body, payload) {
				t.Errorf("HTTP binary payload lost: %d bytes, %v", len(body), err)
			}
			if err := <-forwarded; err != nil {
				t.Errorf("forward HTTP: %v", err)
			}
		}()
	}
	clients.Wait()
}

func TestForwardPortPreservesHalfCloseInBothDirections(t *testing.T) {
	for _, guestFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("guest-first-%v", guestFirst), func(t *testing.T) {
			listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			guestDone := make(chan error, 1)
			go func() {
				connection, err := listener.AcceptTCP()
				if err != nil {
					guestDone <- err
					return
				}
				defer func() { _ = connection.Close() }()
				_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
				if guestFirst {
					_, _ = connection.Write([]byte("ready"))
					_ = connection.CloseWrite()
				}
				body, err := io.ReadAll(connection)
				if err == nil && string(body) != "request" {
					err = fmt.Errorf("guest received %q", body)
				}
				if !guestFirst && err == nil {
					_, err = connection.Write([]byte("response after EOF"))
				}
				guestDone <- err
			}()
			client, local := portTCPPair(t)
			transport := processConnection(t, processServer(t))
			forwarded := make(chan error, 1)
			go func() { forwarded <- ForwardPort(t.Context(), transport, portNumber(t, listener.Addr()), local) }()
			if guestFirst {
				body, err := io.ReadAll(client)
				if err != nil || string(body) != "ready" {
					t.Fatalf("guest half-close: %q, %v", body, err)
				}
			}
			if _, err := io.WriteString(client, "request"); err != nil {
				t.Fatal(err)
			}
			_ = client.CloseWrite()
			if !guestFirst {
				body, err := io.ReadAll(client)
				if err != nil || string(body) != "response after EOF" {
					t.Fatalf("response after request EOF: %q, %v", body, err)
				}
			}
			if err := awaitPort(t, guestDone); err != nil {
				t.Fatal(err)
			}
			if err := awaitPort(t, forwarded); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForwardPortCancellationAndGuestShutdown(t *testing.T) {
	for _, closeGuest := range []bool{false, true} {
		t.Run(fmt.Sprintf("guest-shutdown-%v", closeGuest), func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			client, local := portTCPPair(t)
			server := processServer(t)
			transport := processConnection(t, server)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			forwarded := make(chan error, 1)
			go func() { forwarded <- ForwardPort(ctx, transport, portNumber(t, listener.Addr()), local) }()
			guest, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer func(connection net.Conn) { _ = connection.Close() }(guest)
			if closeGuest {
				_ = server.Close()
			} else {
				cancel()
			}
			err = awaitPort(t, forwarded)
			if err == nil || (!closeGuest && !errors.Is(err, context.Canceled)) {
				t.Fatalf("shutdown error = %v", err)
			}
			if _, err := client.Read(make([]byte, 1)); err == nil {
				t.Fatal("local stream remains open")
			}
		})
	}
}

func TestPortForwardRejectsBadPortsAndUnavailableServices(t *testing.T) {
	server := processServer(t)
	for _, port := range []uint32{0, 65536} {
		connection := processConnection(t, server)
		if err := NewEncoder(connection).Encode(Message{Type: MessagePortForward, Port: port}); err != nil {
			t.Fatal(err)
		}
		reply, err := NewDecoder(connection).Decode()
		if err != nil || reply.Type != MessageError || reply.Code != "INVALID_ARGUMENT" {
			t.Fatalf("bad port %d = %+v, %v", port, reply, err)
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portNumber(t, listener.Addr())
	_ = listener.Close()
	_, local := portTCPPair(t)
	err = ForwardPort(t.Context(), processConnection(t, server), port, local)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "UNAVAILABLE" {
		t.Fatalf("unavailable service = %v", err)
	}
}

func TestPortFramesRejectInvalidOrderingAndPayloads(t *testing.T) {
	for _, messages := range [][]Message{
		{{Type: MessageDone}},
		{{Type: MessagePortEnd}, {Type: MessagePortData, Data: []byte("late")}},
		{{Type: MessagePortEnd}, {Type: MessagePortEnd}},
		{{Type: MessagePortData, Data: make([]byte, streamChunkSize+1)}},
		{{Type: MessageStdout}},
	} {
		var wire bytes.Buffer
		for _, message := range messages {
			if err := NewEncoder(&wire).Encode(message); err != nil {
				t.Fatal(err)
			}
		}
		_, local := portTCPPair(t)
		if err := receivePortData(NewDecoder(&wire), local, portHost, make(chan portEvent, 4)); err == nil {
			t.Fatalf("accepted invalid frames: %q", wire.String()[:min(wire.Len(), 100)])
		}
	}
}

func TestForwardPortReportsOldAgentAndCancelledHandshake(t *testing.T) {
	_, local := portTCPPair(t)
	host, guest := net.Pipe()
	defer func(connection net.Conn) { _ = connection.Close() }(guest)
	go func() {
		_, _ = NewDecoder(guest).Decode()
		_ = NewEncoder(guest).Encode(Message{Type: MessageError, Message: "unknown operation"})
	}()
	if err := ForwardPort(t.Context(), host, 80, local); err == nil || !strings.Contains(err.Error(), "unknown operation") {
		t.Fatalf("old agent = %v", err)
	}
	_, local = portTCPPair(t)
	host, guest = net.Pipe()
	defer func(connection net.Conn) { _ = connection.Close() }(guest)
	ctx, cancel := context.WithCancel(t.Context())
	forwarded := make(chan error, 1)
	go func() { forwarded <- ForwardPort(ctx, host, 80, local) }()
	_, _ = NewDecoder(guest).Decode()
	cancel()
	if err := awaitPort(t, forwarded); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled handshake = %v", err)
	}
}

// shortPortWrite wraps a pipe with a short write deadline, modeling a TCP
// service whose receive buffer remains full after its forwarding peer leaves.
type shortPortWrite struct{ net.Conn }

func (c shortPortWrite) SetWriteDeadline(time.Time) error {
	return c.Conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
}

func TestPortRelayBoundsBlockedWriteAfterPeerDisconnect(t *testing.T) {
	transport, peer := net.Pipe()
	local, stalled := net.Pipe()
	defer func() { _ = stalled.Close() }()
	finished := make(chan error, 1)
	go func() {
		finished <- relayPort(t.Context(), transport, NewDecoder(transport), NewEncoder(transport), shortPortWrite{local}, portGuest)
	}()
	if err := NewEncoder(peer).Encode(Message{Type: MessagePortData, Data: []byte("service never reads this")}); err != nil {
		t.Fatal(err)
	}
	_ = peer.Close()
	var networkError net.Error
	if err := awaitPort(t, finished); !errors.As(err, &networkError) || !networkError.Timeout() {
		t.Fatalf("blocked writer did not time out and release its pumps: %v", err)
	}
}
