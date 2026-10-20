package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kumabox/kumabox/types"
)

const processWaitDelay = 2 * time.Second

// Server accepts one operation per connection. Foreground commands belong to
// their connections; detached commands belong to the server's bounded table.
type Server struct {
	listener net.Listener
	logger   *log.Logger

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool
	processes   *processManager
	closeDone   chan struct{}
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
	return &Server{listener: listener, logger: logger, connections: make(map[net.Conn]struct{}), processes: newProcessManager(), closeDone: make(chan struct{})}, nil
}

// Serve handles sessions until ctx is canceled or the listener fails.
func (s *Server) Serve(ctx context.Context) error {
	defer s.Close() //nolint:errcheck // active sessions and process ownership are always released
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
		if s.closeDone != nil {
			<-s.closeDone
		}
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
	if s.processes != nil {
		s.processes.close()
	}
	if s.closeDone != nil {
		close(s.closeDone)
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
	case MessagePortForward:
		if err := servePort(ctx, connection, decoder, encoder, first.Port); err != nil {
			s.logger.Printf("forward guest port: %v", err)
		}
	case MessageExec, MessageExecV2:
		s.runCommand(ctx, connection, decoder, encoder, first)
	case MessageSpawn, MessageProcesses, MessageSignal, MessageLogs, MessageAttach:
		s.handleProcess(ctx, connection, decoder, encoder, first)
	case MessageFileRead, MessageFileWrite, MessageFileList, MessageFileStat, MessageFileMkdir, MessageFileRemove, MessageFileRename:
		if err := serveFile(ctx, decoder, encoder, first); err != nil {
			fileFailure(encoder, err)
		}
	case MessageReseed:
		if err := reseedGuest(first.Data, first.RegenMachineID); err != nil {
			_ = encoder.sendError("reseed: %v", err)
			return
		}
		_ = encoder.Encode(Message{Type: MessageExit})
	case MessageConfigureNetwork:
		if first.Network == nil {
			_ = encoder.sendError("configure network: missing configuration")
			return
		}
		if err := applyGuestNetwork(ctx, *first.Network); err != nil {
			_ = encoder.sendError("configure network: %v", err)
			return
		}
		_ = encoder.Encode(Message{Type: MessageExit})
	default:
		_ = encoder.sendError("expected first frame type %q, got %q", MessageExec, first.Type)
	}
}

func (s *Server) runCommand(parent context.Context, connection net.Conn, decoder *Decoder, encoder *Encoder, request Message) {
	if err := request.command().Validate(); err != nil {
		_ = encoder.sendError("exec: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	command := exec.CommandContext(ctx, request.Argv[0], request.Argv[1:]...) //nolint:gosec // argv comes from the owner-only host channel and is never passed through a shell
	if err := prepareCommand(command, request); err != nil {
		_ = encoder.sendError("exec: %v", err)
		return
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
	inputDone := make(chan error, 1)
	go func() {
		inputErr := receiveInput(ctx, decoder, input)
		if inputErr != nil {
			cancel()
		}
		inputDone <- inputErr
	}()
	if err := encoder.Encode(Message{Type: MessageStarted, PID: command.Process.Pid}); err != nil {
		cancel()
		_ = command.Wait()
		_ = connection.SetReadDeadline(time.Now())
		<-inputDone
		return
	}
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

func (m Message) command() types.Command {
	return types.Command{Args: m.Argv, Env: m.Env, Cwd: m.Cwd, User: m.User}
}

// prepareCommand is shared by foreground and detached execution. Identity is
// configured using OS credentials, never a shell or su wrapper. Explicit env
// overrides take precedence over the selected user's HOME/USER/LOGNAME.
func prepareCommand(command *exec.Cmd, request Message) error {
	if err := request.command().Validate(); err != nil {
		return err
	}
	configureProcess(command)
	command.WaitDelay = processWaitDelay
	command.Dir = request.Cwd
	defaults, err := configureIdentity(command, request.User)
	if err != nil {
		return fmt.Errorf("select guest user: %w", err)
	}
	if len(defaults) > 0 {
		command.Env = mergeEnvironment(command.Environ(), defaults)
	}
	if len(request.Env) > 0 {
		command.Env = mergeEnvironment(command.Environ(), request.Env)
	}
	return nil
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
			if inputOpen {
				_ = input.Close()
				inputOpen = false
			}
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
