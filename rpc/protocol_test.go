package rpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/errdefs"
)

func testClient(t *testing.T, mux http.Handler) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kb-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{Handler: mux, Protocols: protocols, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	client := NewClient(socket)
	t.Cleanup(func() { _ = client.Close(); _ = server.Close(); <-done; _ = os.RemoveAll(dir) })
	return client
}

func TestCallStreamsBeforeInputEOF(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, "duplex", func(_ context.Context, s *Session, _ struct{}) (any, error) {
		if _, err := s.Output("stdout").Write([]byte{0, 1, 255}); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(s.Input)
		if err != nil {
			return nil, err
		}
		if _, err := s.Output("stderr").Write([]byte("diagnostic")); err != nil {
			return nil, err
		}
		return string(data), nil
	})
	client := testClient(t, mux)
	input, writer := io.Pipe()
	defer func() { _ = input.Close(); _ = writer.Close() }()
	received := make(chan struct{})
	var once sync.Once
	out := writeFunc(func(p []byte) (int, error) {
		if !bytes.Equal(p, []byte{0, 1, 255}) {
			t.Errorf("stdout=%v", p)
		}
		once.Do(func() { close(received) })
		return len(p), nil
	})
	go func() {
		select {
		case <-received:
			_, _ = writer.Write([]byte("input"))
		case <-time.After(3 * time.Second):
		}
		_ = writer.Close()
	}()
	var result string
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.Call(ctx, "duplex", struct{}{}, &result, Streams{Input: input, Output: out, Error: &stderr}); err != nil {
		t.Fatal(err)
	}
	if result != "input" || stderr.String() != "diagnostic" {
		t.Fatalf("result=%q stderr=%q", result, stderr.String())
	}
}

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

func TestCallPreservesPartialCommittedFailure(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, "partial", func(_ context.Context, _ *Session, _ struct{}) (any, error) {
		return 42, &errdefs.Error{Class: errdefs.ClassConflict, Code: errdefs.CodeStateConflict, Committed: true, Cause: errors.New("committed before cleanup failed")}
	})
	client := testClient(t, mux)
	var result int
	err := client.Call(t.Context(), "partial", struct{}{}, &result, Streams{})
	var classified *errdefs.Error
	if result != 42 || !errors.As(err, &classified) || !classified.Committed || classified.Code != errdefs.CodeStateConflict {
		t.Fatalf("result=%d error=%v", result, err)
	}
}

func TestCallCancellationAndCallbackFailureReachServer(t *testing.T) {
	for _, callback := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "callback"}[callback], func(t *testing.T) {
			mux := http.NewServeMux()
			started := make(chan struct{})
			stopped := make(chan struct{})
			Register(mux, "wait", func(ctx context.Context, s *Session, _ struct{}) (any, error) {
				close(started)
				_ = s.Emit("status", "waiting")
				<-ctx.Done()
				close(stopped)
				return nil, ctx.Err()
			})
			client := testClient(t, mux)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			streams := Streams{}
			if callback {
				streams.Event = func(Frame) error { return errors.New("renderer failed") }
			}
			go func() { done <- client.Call(ctx, "wait", struct{}{}, nil, streams) }()
			<-started
			if !callback {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("missing failure")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("client did not stop")
			}
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("server request leaked")
			}
		})
	}
}

func TestConcurrentCallsKeepRequestEventsIndependent(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, "echo", func(_ context.Context, s *Session, value int) (any, error) {
		if err := s.Emit("status", value); err != nil {
			return nil, err
		}
		return value, nil
	})
	client := testClient(t, mux)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var result int
			events := 0
			err := client.Call(t.Context(), "echo", i, &result, Streams{Event: func(frame Frame) error { events++; return nil }})
			if err != nil || result != i || events != 1 {
				t.Errorf("request %d: result=%d events=%d err=%v", i, result, events, err)
			}
		}()
	}
	wg.Wait()
}

func TestMissingDaemonDoesNotOpenRuntimeState(t *testing.T) {
	dir := t.TempDir()
	client := NewClient(filepath.Join(dir, "missing.sock"))
	defer func() { _ = client.Close() }()
	err := client.Call(t.Context(), "anything", nil, nil, Streams{})
	if !IsUnavailable(err) {
		t.Fatalf("error=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("client created local state: %v, %v", entries, err)
	}
}

func TestRequestAndResponseBounds(t *testing.T) {
	if _, err := EncodeRequest(string(bytes.Repeat([]byte("x"), maxMetadata)), time.Time{}); err == nil {
		t.Fatal("oversized request accepted")
	}
	scanner := Scanner(bytes.NewReader(bytes.Repeat([]byte("x"), maxFrame+1)))
	if _, err := ReadFrame(scanner); err == nil {
		t.Fatal("oversized frame accepted")
	}
}
