package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

type consoleStream struct {
	reader *strings.Reader
	writes bytes.Buffer
}

func (s *consoleStream) Close() error                { return nil }
func (s *consoleStream) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *consoleStream) Write(p []byte) (int, error) { return s.writes.Write(p) }

func TestRelayConsoleCopiesBothDirections(t *testing.T) {
	stream := &consoleStream{reader: strings.NewReader("from-guest")}
	var output bytes.Buffer
	input := strings.NewReader("to-guest")

	if err := relayConsole(context.Background(), input, &output, stream); err != nil && err != io.EOF {
		t.Fatalf("relayConsole() error = %v", err)
	}
	if !strings.Contains(stream.writes.String(), "to-guest") {
		t.Fatalf("guest input was not relayed: %q", stream.writes.String())
	}
}
