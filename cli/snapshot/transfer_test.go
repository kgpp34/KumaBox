package snapshot

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/rpc"
)

func TestImportCancellationClosesIdleInheritedStdin(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "kb-import-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	socket := filepath.Join(directory, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	started, stopped := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	rpc.Register(mux, "snapshot.import", func(_ context.Context, s *rpc.Session, _ rpc.SnapshotImport) (any, error) {
		close(started)
		_, err := io.Copy(io.Discard, s.Input)
		close(stopped)
		return nil, err
	})
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{Protocols: protocols, Handler: mux, ReadHeaderTimeout: time.Second}
	serving := make(chan error, 1)
	go func() { serving <- server.Serve(listener) }()
	defer func() { _ = server.Close(); <-serving }()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	_ = reader.Fd() // Match a shell's inherited, blocking stdin descriptor.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := config.Default()
	cfg.Client.Socket = socket
	command := NewCommand(func() config.Config { return cfg })
	command.SetArgs([]string{"import"})
	command.SetIn(reader)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() { done <- command.ExecuteContext(ctx) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upload did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stdin cancellation hung")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon upload leaked")
	}
}
