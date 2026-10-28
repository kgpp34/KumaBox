package sandbox

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/daemon"
)

// Short paths keep Unix socket names within the kernel's sockaddr_un limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/tmp", "kb-node-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}

var testNodes = struct {
	sync.Mutex
	running map[string]bool
}{running: make(map[string]bool)}

func startTestNode(t *testing.T, cfg config.Config) {
	t.Helper()
	testNodes.Lock()
	defer testNodes.Unlock()
	socket := cfg.SocketPath()
	if testNodes.running[socket] {
		return
	}
	testNodes.running[socket] = true
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, cfg, daemon.Options{Ready: func(string) error { close(ready); return nil }}, io.Discard)
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("start test node: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("node startup timed out")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("node shutdown timed out")
		}
		testNodes.Lock()
		delete(testNodes.running, socket)
		testNodes.Unlock()
	})
}
