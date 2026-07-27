package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
)

type consoleStream struct {
	mu        sync.Mutex
	readData  []byte
	readReady chan struct{}
	readyOnce sync.Once
	writes    bytes.Buffer
}

func (s *consoleStream) Close() error { return nil }

func (s *consoleStream) Read(p []byte) (int, error) {
	<-s.readReady
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.readData) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.readData)
	s.readData = s.readData[n:]
	return n, nil
}

func (s *consoleStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.writes.Write(p)
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.readReady) })
	return n, err
}

type recordingWriter struct {
	mu   sync.Mutex
	data bytes.Buffer
	done chan struct{}
	once sync.Once
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.data.Write(p)
	w.mu.Unlock()
	w.once.Do(func() { close(w.done) })
	return n, err
}

func (w *recordingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}

func TestRelayConsoleCopiesBothDirections(t *testing.T) {
	stream := &consoleStream{readData: []byte("from-guest"), readReady: make(chan struct{})}
	output := &recordingWriter{done: make(chan struct{})}
	input := strings.NewReader("to-guest")

	if err := relayConsole(context.Background(), input, output, stream); err != nil && err != io.EOF {
		t.Fatalf("relayConsole() error = %v", err)
	}
	<-output.done
	stream.mu.Lock()
	guestInput := stream.writes.String()
	stream.mu.Unlock()
	if !strings.Contains(guestInput, "to-guest") {
		t.Fatalf("guest input was not relayed: %q", guestInput)
	}
	if output.String() != "from-guest" {
		t.Fatalf("guest output was not relayed: %q", output.String())
	}
}
