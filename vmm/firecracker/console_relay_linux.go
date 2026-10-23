//go:build linux

package firecracker

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// serialStream requires cancellable, bounded writes. Both Linux PTYs and test
// network streams implement this contract without a VMM-specific interface.
type serialStream interface {
	io.ReadWriteCloser
	SetWriteDeadline(time.Time) error
}

// serialRelay owns one serial reader and at most one console input goroutine.
// It drains and logs output even without an attached client. A stalled client
// is disconnected after a bounded write; it cannot indefinitely stall the VM.
//
// Firecracker <-> PTY <-> relay <-> console.sock <-> CLI
//
//	|
//	+---- vmm.log (always drained)
type serialRelay struct {
	serial   serialStream
	listener net.Listener
	log      io.Writer
	mu       sync.Mutex
	client   net.Conn
}

func (r *serialRelay) detach(client net.Conn) {
	r.mu.Lock()
	if r.client == client {
		r.client = nil
	}
	r.mu.Unlock()
	_ = client.Close()
}

func (r *serialRelay) close() {
	_ = r.listener.Close()
	_ = r.serial.Close()
	r.mu.Lock()
	client := r.client
	r.client = nil
	r.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

func (r *serialRelay) readSerial() error {
	buffer := make([]byte, 4096)
	for {
		n, err := r.serial.Read(buffer)
		if n > 0 {
			written, logErr := r.log.Write(buffer[:n])
			if logErr != nil {
				return logErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			r.mu.Lock()
			client := r.client
			r.mu.Unlock()
			if client != nil {
				writeErr := client.SetWriteDeadline(time.Now().Add(consoleIOTimeout))
				if writeErr == nil {
					var written int
					written, writeErr = client.Write(buffer[:n])
					if writeErr == nil && written != n {
						writeErr = io.ErrShortWrite
					}
				}
				if writeErr != nil {
					_ = client.Close()
				}
			}
		}
		if err != nil {
			return err
		}
	}
}

func (r *serialRelay) readClient(client net.Conn) {
	defer r.detach(client)
	buffer := make([]byte, 4096)
	for {
		n, err := client.Read(buffer)
		if n > 0 {
			if err := r.serial.SetWriteDeadline(time.Now().Add(consoleIOTimeout)); err != nil {
				return
			}
			if written, err := r.serial.Write(buffer[:n]); err != nil || written != n {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// serve joins every I/O goroutine before returning. Closing the PTY and client
// interrupts blocked I/O; accepting a second client never spawns another worker.
func (r *serialRelay) serve(parent context.Context, ready io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	stop := context.AfterFunc(ctx, r.close)
	defer func() { cancel(); r.close(); workers.Wait(); stop() }()
	readResult := make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); readResult <- r.readSerial(); cancel() }()
	if _, err := ready.Write([]byte{consoleReady}); err != nil {
		return err
	}
	for {
		client, err := r.listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				return err
			}
			select {
			case err := <-readResult:
				if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, syscall.EIO) {
					return err
				}
			default:
			}
			return nil
		}
		_ = client.SetWriteDeadline(time.Now().Add(consoleIOTimeout))
		r.mu.Lock()
		if r.client != nil {
			r.mu.Unlock()
			_, _ = client.Write([]byte{consoleBusy})
			_ = client.Close()
			continue
		}
		if _, err := client.Write([]byte{consoleReady}); err != nil {
			r.mu.Unlock()
			_ = client.Close()
			continue
		}
		r.client = client
		r.mu.Unlock()
		workers.Add(1)
		go func() { defer workers.Done(); r.readClient(client) }()
	}
}
