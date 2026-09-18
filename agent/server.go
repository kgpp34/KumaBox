package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const processWaitDelay = 2 * time.Second

// Server accepts independent agent sessions. Each connection executes exactly
// one operation and owns one child-process tree.
type Server struct {
	listener net.Listener
	logger   *log.Logger

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool
}

// NewServer constructs a guest agent around listener. A nil logger discards
// diagnostics so protocol output never shares the command data channel.
func NewServer(listener net.Listener, logger *log.Logger) (*Server, error) {
	if listener == nil {
		return nil, errors.New("agent listener is required")
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{listener: listener, logger: logger, connections: make(map[net.Conn]struct{})}, nil
}

// Serve handles sessions until ctx is canceled or the listener fails.
func (s *Server) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()

	var sessions sync.WaitGroup
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				sessions.Wait()
				return nil
			}
			_ = s.Close()
			sessions.Wait()
			return fmt.Errorf("accept agent connection: %w", err)
		}
		if !s.track(connection) {
			_ = connection.Close()
			continue
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			s.handle(ctx, connection)
		}()
	}
}

// Close stops accepting sessions and unblocks active handlers.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	connections := make([]net.Conn, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.mu.Unlock()

	err := s.listener.Close()
	for _, connection := range connections {
		err = errors.Join(err, connection.Close())
	}
	return err
}

func (s *Server) handle(ctx context.Context, connection net.Conn) {
	defer s.untrack(connection)
	defer connection.Close() //nolint:errcheck

	decoder := NewDecoder(connection)
	encoder := NewEncoder(connection)
	first, err := decoder.Decode()
	if err != nil {
		if !errors.Is(err, io.EOF) {
			s.logger.Printf("decode initial frame from %s: %v", connection.RemoteAddr(), err)
		}
		return
	}
	switch first.Type {
	case MessageExec:
		s.runCommand(ctx, connection, decoder, encoder, first)
	case MessageReseed:
		_ = encoder.sendError("reseed is not implemented by this KumaBox agent")
	default:
		_ = encoder.sendError("expected first frame type %q, got %q", MessageExec, first.Type)
	}
}

func (s *Server) runCommand(parent context.Context, connection net.Conn, decoder *Decoder, encoder *Encoder, request Message) {
	if len(request.Argv) == 0 || request.Argv[0] == "" {
		_ = encoder.sendError("exec: argv is empty")
		return
	}
	for key, value := range request.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			_ = encoder.sendError("exec: invalid environment variable %q", key)
			return
		}
	}
	for _, argument := range request.Argv {
		if strings.IndexByte(argument, 0) >= 0 {
			_ = encoder.sendError("exec: command arguments contain a NUL byte")
			return
		}
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	command := exec.CommandContext(ctx, request.Argv[0], request.Argv[1:]...) //nolint:gosec // argv comes from the owner-only host channel and is never passed through a shell
	command.WaitDelay = processWaitDelay
	configureProcess(command)
	if len(request.Env) > 0 {
		command.Env = mergeEnvironment(os.Environ(), request.Env)
	}
	input, err := command.StdinPipe()
	if err != nil {
		_ = encoder.sendError("exec: open stdin: %v", err)
		return
	}
	stdout := &framedWriter{messageType: MessageStdout, encoder: encoder, cancel: cancel}
	stderr := &framedWriter{messageType: MessageStderr, encoder: encoder, cancel: cancel}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		_ = input.Close()
		_ = encoder.sendError("exec: start %s: %v", request.Argv[0], err)
		return
	}
	if err := encoder.Encode(Message{Type: MessageStarted, PID: command.Process.Pid}); err != nil {
		cancel()
		_ = command.Wait()
		_ = input.Close()
		return
	}

	inputDone := make(chan error, 1)
	go func() {
		inputErr := receiveInput(ctx, decoder, input)
		if inputErr != nil {
			cancel()
		}
		inputDone <- inputErr
	}()
	waitErr := command.Wait()
	cancel()
	_ = connection.SetReadDeadline(time.Now())
	inputErr := <-inputDone

	if outputErr := errors.Join(stdout.err(), stderr.err()); outputErr != nil {
		s.logger.Printf("stream command output: %v", outputErr)
		return
	}
	if inputErr != nil {
		_ = encoder.sendError("exec: receive stdin: %v", inputErr)
		return
	}
	exitCode := 0
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		exitCode = processExitCode(exitErr.ProcessState)
	case errors.Is(waitErr, exec.ErrWaitDelay) && command.ProcessState != nil:
		exitCode = processExitCode(command.ProcessState)
	default:
		_ = encoder.sendError("exec: wait %s: %v", request.Argv[0], waitErr)
		return
	}
	if err := encoder.Encode(Message{Type: MessageExit, ExitCode: exitCode}); err != nil {
		s.logger.Printf("send command exit: %v", err)
	}
}

// mergeEnvironment removes inherited values that the request overrides and
// appends the replacements in stable order. The child therefore receives one
// unambiguous value for every environment key.
func mergeEnvironment(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, pair := range base {
		key, _, ok := strings.Cut(pair, "=")
		if _, replaced := overrides[key]; ok && replaced {
			continue
		}
		result = append(result, pair)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

func receiveInput(ctx context.Context, decoder *Decoder, input io.WriteCloser) error {
	defer input.Close() //nolint:errcheck
	inputOpen := true
	for {
		message, err := decoder.Decode()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return errors.New("host connection closed")
			}
			return err
		}
		switch message.Type {
		case MessageStdinClose:
			return nil
		case MessageStdin:
			if len(message.Data) == 0 || !inputOpen {
				continue
			}
			if _, err := input.Write(message.Data); err != nil {
				_ = input.Close()
				inputOpen = false
			}
		default:
			return fmt.Errorf("unexpected frame type %q", message.Type)
		}
		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
}

func (s *Server) track(connection net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.connections[connection] = struct{}{}
	return true
}

func (s *Server) untrack(connection net.Conn) {
	s.mu.Lock()
	delete(s.connections, connection)
	s.mu.Unlock()
}
