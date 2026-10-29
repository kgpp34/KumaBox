package sandbox

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/config"
)

func TestForwardRejectsInvalidOptionsBeforeOpeningService(t *testing.T) {
	for _, args := range [][]string{
		{"box", "80"},
		{"box", "1:0"},
		{"box", "65536:80"},
		{"box", "-1:80"},
		{"box", "80:65536"},
		{"box", "80:80:80"},
		{"box", "80:80", "--address", "0.0.0.0"},
		{"box", "80:80", "--address", "example.com"},
	} {
		command := NewForwardCommand(func() config.Config { t.Fatal("invalid forwarding opened metadata"); return config.Config{} })
		command.SetArgs(args)
		command.SilenceUsage, command.SilenceErrors = true, true
		if err := command.ExecuteContext(t.Context()); err == nil {
			t.Fatalf("accepted invalid arguments %q", args)
		}
	}
	local, guest, err := parseForwardPorts("0:65535")
	if err != nil || local != 0 || guest != 65535 {
		t.Fatalf("ephemeral host port = %d:%d, %v", local, guest, err)
	}
}

func TestForwardListenerCancelsActiveConnectionsAndRejectsExcess(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	accepted := make(chan struct{}, maxForwardConnections)
	reported := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveForward(ctx, listener, func(_ context.Context, connection net.Conn) error {
			accepted <- struct{}{}
			_, err := io.Copy(io.Discard, connection)
			return err
		}, func(err error) { reported <- err })
	}()
	for range maxForwardConnections {
		connection, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		select {
		case <-accepted:
		case <-time.After(3 * time.Second):
			t.Fatal("forward connection was not accepted")
		}
	}
	excess, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = excess.Close() }()
	_ = excess.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := excess.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("excess connection was not closed: %v", err)
	}
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "limit") {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capacity error was not reported")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listener or active handlers leaked after cancellation")
	}
	if connection, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second); err == nil {
		_ = connection.Close()
		t.Fatal("listener remains open")
	}
}
