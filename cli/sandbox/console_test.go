package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestConsoleEscapeCharacters(t *testing.T) {
	for _, test := range []struct {
		input string
		want  byte
	}{
		{input: "^]", want: 0x1d},
		{input: "^a", want: 0x01},
		{input: "x", want: 'x'},
	} {
		got, err := parseEscapeChar(test.input)
		if err != nil {
			t.Fatalf("parseEscapeChar(%q): %v", test.input, err)
		}
		if got != test.want || formatEscapeChar(got) == "" {
			t.Fatalf("parseEscapeChar(%q) = %#x, want %#x", test.input, got, test.want)
		}
	}
	for _, invalid := range []string{"", "ab", "^?", "\n", string([]byte{0x80})} {
		if _, err := parseEscapeChar(invalid); err == nil {
			t.Fatalf("accepted escape character %q", invalid)
		}
	}
}

func TestRelayConsoleDetachesWithoutForwardingEscapeSequence(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	received := make(chan string, 1)
	go func() {
		data := make([]byte, 5)
		_, err := io.ReadFull(server, data)
		if err != nil {
			received <- "error: " + err.Error()
			return
		}
		received <- string(data)
	}()

	input := bytes.NewReader([]byte{'h', 'e', 'l', 'l', 'o', 0x1d, '.'})
	if err := relayConsole(t.Context(), client, input, io.Discard, []byte{0x1d, '.'}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "hello" {
			t.Fatalf("remote received %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("remote did not receive console input")
	}
}

func TestRelayConsoleCancellationClosesRemote(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	input, writer := io.Pipe()
	t.Cleanup(func() { _ = input.Close() })
	t.Cleanup(func() { _ = writer.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := relayConsole(ctx, client, input, io.Discard, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("relay cancellation error = %v", err)
	}
}
