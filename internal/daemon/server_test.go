package daemon

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/client"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/storage"
)

func nodeConfig(t *testing.T) config.Config {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kb-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := config.Default()
	cfg.Paths = storage.Roots{Data: filepath.Join(dir, "data"), Run: filepath.Join(dir, "run"), Log: filepath.Join(dir, "log")}
	cfg.Network.CNI.ConfDir = filepath.Join(dir, "cni")
	return cfg
}

func runningNode(t *testing.T, cfg config.Config) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, Options{Ready: func(string) error { close(ready); return nil }}, io.Discard)
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("start node: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("start timed out")
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("stop timed out")
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestRunOwnsDataRootAndRestarts(t *testing.T) {
	cfg := nodeConfig(t)
	stop := runningNode(t, cfg)
	info, err := os.Stat(cfg.SocketPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions=%v %v", info, err)
	}
	images, err := client.OpenImages(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = images.Close() }()
	if records, err := images.List(t.Context()); err != nil || len(records) != 0 {
		t.Fatalf("list=%v %v", records, err)
	}
	second := cfg
	second.Client.Socket = filepath.Join(cfg.Paths.Run, "second.sock")
	if err := Run(t.Context(), second, Options{}, io.Discard); err == nil {
		t.Fatal("second writer accepted")
	}
	stop()
	if _, err := os.Lstat(cfg.SocketPath()); !os.IsNotExist(err) {
		t.Fatalf("socket remained after shutdown: %v", err)
	}
	runningNode(t, cfg)
	if _, err := images.List(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRunPreservesNonSocketPaths(t *testing.T) {
	cfg := nodeConfig(t)
	if err := os.MkdirAll(cfg.Paths.Run, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.SocketPath(), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), cfg, Options{}, io.Discard); err == nil {
		t.Fatal("regular file overwritten")
	}
	data, err := os.ReadFile(cfg.SocketPath())
	if err != nil || string(data) != "keep" {
		t.Fatalf("file changed: %q %v", data, err)
	}
}

func TestNotifyReadyUsesSystemdEndpoint(t *testing.T) {
	cfg := nodeConfig(t)
	address := filepath.Join(filepath.Dir(cfg.Paths.Data), "notify")
	connection, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: address, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := notifyReady(address); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	message := make([]byte, 1024)
	n, err := connection.Read(message)
	if err != nil || !strings.HasPrefix(string(message[:n]), "READY=1\n") {
		t.Fatalf("readiness=%q %v", message[:n], err)
	}
}
