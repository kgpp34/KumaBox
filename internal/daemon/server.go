// Package daemon owns the single local runtime instance and its node socket.
//
// CLI -> HTTP/2 Unix socket -> shared Application -> VMM / disk / network
//
//	       |
//	supervisor + recovery
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/core"
	"github.com/kumabox/kumabox/internal/errdefs"
	filelock "github.com/kumabox/kumabox/internal/lock/flock"
	"github.com/kumabox/kumabox/internal/rpc"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/version"
)

// Options controls node maintenance and shutdown, independently of guest shape.
type Options struct {
	ReconcileInterval time.Duration
	GCInterval        time.Duration
	ShutdownTimeout   time.Duration
	// MaxOperations bounds active requests without serializing independent VMs.
	MaxOperations int
	// NotifySocket enables systemd readiness after recovery and socket setup.
	NotifySocket string
	// Ready runs after the socket is secured and recovery has completed.
	Ready func(string) error
}

// NewHandler binds all runtime capabilities to an already opened application.
func NewHandler(app *core.Application, cfg config.Config) http.Handler {
	mux := http.NewServeMux()
	registerOperations(mux, app)
	registerStreams(mux, app)
	registerImages(mux, app)
	registerDoctor(mux)
	rpc.Register(mux, "node.info", func(context.Context, *rpc.Session, struct{}) (any, error) {
		return types.NodeInfo{Version: version.String(), DataRoot: cfg.Paths.Data, RunRoot: cfg.Paths.Run, LogRoot: cfg.Paths.Log}, nil
	})
	return mux
}

// Run holds exclusive ownership of the data root and socket until all requests
// and maintenance work have stopped. Existing VMs survive a service shutdown.
func Run(ctx context.Context, cfg config.Config, options Options, diagnostics io.Writer) (returnErr error) {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if options.ReconcileInterval == 0 {
		options.ReconcileInterval = 5 * time.Second
	}
	if options.MaxOperations == 0 {
		options.MaxOperations = 256
	}
	if options.MaxOperations < 1 {
		return errors.New("max operations must be positive")
	}
	if options.ShutdownTimeout == 0 {
		options.ShutdownTimeout = 30 * time.Second
	}
	if options.ReconcileInterval < 0 || options.GCInterval < 0 || options.ShutdownTimeout < 0 {
		return errors.New("daemon intervals must be positive")
	}
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	socket := cfg.SocketPath()
	if err := os.MkdirAll(filepath.Join(cfg.Paths.Data, "meta"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	parent, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		return err
	}
	if parent.Mode().Perm()&0o022 != 0 {
		return errors.New("daemon socket directory must not be writable by group or others")
	}
	for _, path := range []string{filepath.Join(cfg.Paths.Data, "meta", "node.lock"), socket + ".lock"} {
		lock := filelock.New(path)
		acquired, err := lock.TryLock(ctx)
		if err != nil {
			return err
		}
		if !acquired {
			return errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, fmt.Errorf("another KumaBox daemon owns %s", path))
		}
		defer func() { returnErr = errors.Join(returnErr, lock.Unlock(context.WithoutCancel(ctx))) }()
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refuse to replace non-socket path %s", socket)
		}
		connection, dialErr := net.DialTimeout("unix", socket, time.Second)
		if dialErr == nil {
			_ = connection.Close()
			return errors.New("another service is listening on the node socket")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	application, err := core.OpenApplication(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, application.Close()) }()
	if _, _, err := application.Maintenance.ReconcileSandboxes(ctx); err != nil {
		return fmt.Errorf("recover node state: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	nodeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	var requests sync.WaitGroup
	var requestMu sync.Mutex
	closing := false
	// Freeze admission before Wait: HTTP/2 handlers can be scheduled while
	// connections are closing, but must never start work against a closed store.
	stopAdmission := func() {
		requestMu.Lock()
		closing = true
		requestMu.Unlock()
	}
	admission := make(chan struct{}, options.MaxOperations)
	handler := NewHandler(application, cfg)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{
		Protocols: protocols, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return nodeCtx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case admission <- struct{}{}:
				defer func() { <-admission }()
			default:
				http.Error(w, "node operation limit reached", http.StatusTooManyRequests)
				return
			}
			requestMu.Lock()
			if closing {
				requestMu.Unlock()
				http.Error(w, "node is shutting down", http.StatusServiceUnavailable)
				return
			}
			requests.Add(1)
			requestMu.Unlock()
			defer requests.Done()
			handler.ServeHTTP(w, r)
		}),
	}
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- Supervise(nodeCtx, application.Maintenance, options.ReconcileInterval, options.GCInterval, diagnostics)
	}()
	accessDone := make(chan struct{})
	go func() {
		defer close(accessDone)
		flushSnapshotAccess(nodeCtx, cfg.Snapshots.AccessFlushInterval, application.FlushSnapshotAccess, diagnostics)
	}()
	serving := make(chan error, 1)
	go func() { serving <- server.Serve(listener) }()
	ready := func() error {
		if options.Ready != nil {
			if err := options.Ready(socket); err != nil {
				return err
			}
		}
		return notifyReady(options.NotifySocket)
	}
	if err := ready(); err != nil {
		stopAdmission()
		cancel()
		_ = server.Close()
		<-serving
		requests.Wait()
		<-accessDone
		return errors.Join(err, <-maintenanceDone)
	}
	select {
	case <-ctx.Done():
	case err = <-serving:
		if !errors.Is(err, http.ErrServerClosed) {
			returnErr = err
		}
	}
	stopAdmission()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), options.ShutdownTimeout)
	shutdownErr := server.Shutdown(shutdownCtx)
	shutdownCancel()
	cancel()
	_ = server.Close()
	requests.Wait()
	maintenanceErr := <-maintenanceDone
	<-accessDone
	if errors.Is(shutdownErr, context.DeadlineExceeded) {
		shutdownErr = nil
	}
	return errors.Join(returnErr, maintenanceErr, shutdownErr)
}

// notifyReady uses systemd's existing Unix datagram endpoint without spawning
// systemd-notify or adding a service manager dependency to foreground use.
func notifyReady(socket string) error {
	if socket == "" {
		return nil
	}
	connection, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("notify systemd readiness: %w", err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err = connection.Write([]byte("READY=1\nSTATUS=Serving local runtime requests"))
	return err
}
