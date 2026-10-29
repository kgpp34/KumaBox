package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/types"
)

func processServer(t *testing.T) *Server {
	t.Helper()
	server, err := NewServer(newQueueListener(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func processConnection(t *testing.T, server *Server) net.Conn {
	t.Helper()
	client, guest := net.Pipe()
	if !server.track(guest) {
		t.Fatal("test agent is closed")
	}
	done := make(chan struct{})
	go func() { defer close(done); server.handle(t.Context(), guest) }()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("agent request handler leaked")
		}
	})
	return client
}

func spawnProcess(t *testing.T, server *Server, command types.Command) types.ProcessInfo {
	t.Helper()
	info, err := Spawn(t.Context(), processConnection(t, server), command)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestDetachedProcessSurvivesDisconnectAndReplaysOutput(t *testing.T) {
	server := processServer(t)
	info := spawnProcess(t, server, types.Command{Args: []string{"sh", "-c", "printf out; printf err >&2; exit 7"}})
	var stdout, stderr bytes.Buffer
	result, err := AttachProcess(t.Context(), processConnection(t, server), info.ID, types.ProcessOutputOptions{}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == nil || *result.ExitCode != 7 || stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("attach = %+v, stdout=%q stderr=%q", result, stdout.String(), stderr.String())
	}
	var replay bytes.Buffer
	logs, err := ProcessLogs(t.Context(), processConnection(t, server), info.ID, types.ProcessOutputOptions{After: result.Cursor}, &replay, &replay)
	if err != nil || replay.Len() != 0 || logs.Cursor != result.Cursor {
		t.Fatalf("resume logs = %+v, %q, %v", logs, replay.String(), err)
	}
	records, err := Processes(t.Context(), processConnection(t, server))
	if err != nil || len(records) != 1 || records[0].Running || records[0].ExitCode == nil || *records[0].ExitCode != 7 {
		t.Fatalf("retained records = %+v, %v", records, err)
	}
}

func TestDetachedProcessSignalAndServerClose(t *testing.T) {
	for _, operation := range []string{"signal", "server close"} {
		t.Run(operation, func(t *testing.T) {
			server := processServer(t)
			info := spawnProcess(t, server, types.Command{Args: []string{"sleep", "30"}})
			if operation == "signal" {
				if err := SignalProcess(t.Context(), processConnection(t, server), info.ID, types.ProcessSignalKILL); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := server.Close(); err != nil {
					t.Fatal(err)
				}
			}
			job, err := server.processes.lookup(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			for {
				_, status, _, changed := job.output(0)
				if !status.Running {
					if status.ExitCode == nil || *status.ExitCode == 0 {
						t.Fatalf("killed status = %+v", status)
					}
					break
				}
				select {
				case <-changed:
				case <-deadline.C:
					t.Fatal("process was not reaped")
				}
			}
			if err := job.signal(types.ProcessSignalKILL); !errors.Is(err, errProcessExited) {
				t.Fatalf("signaling exited handle: %v", err)
			}
		})
	}
}

func TestAttachDisconnectDoesNotKillBackgroundCommand(t *testing.T) {
	server := processServer(t)
	info := spawnProcess(t, server, types.Command{Args: []string{"sh", "-c", "printf ready; exec sleep 30"}})
	connection := processConnection(t, server)
	if err := NewEncoder(connection).Encode(Message{Type: MessageAttach, ProcessID: info.ID}); err != nil {
		t.Fatal(err)
	}
	message, err := NewDecoder(connection).Decode()
	if err != nil || message.Type != MessageStdout || string(message.Data) != "ready" {
		t.Fatalf("initial output = %+v, %v", message, err)
	}
	_ = connection.Close()
	records, err := Processes(t.Context(), processConnection(t, server))
	if err != nil || len(records) != 1 || !records[0].Running {
		t.Fatalf("disconnected process = %+v, %v", records, err)
	}
	if err := SignalProcess(t.Context(), processConnection(t, server), info.ID, types.ProcessSignalKILL); err != nil {
		t.Fatal(err)
	}
	result, err := AttachProcess(t.Context(), processConnection(t, server), info.ID, types.ProcessOutputOptions{After: message.Cursor}, io.Discard, io.Discard)
	if err != nil || result.ExitCode == nil || *result.ExitCode == 0 {
		t.Fatalf("reattach result = %+v, %v", result, err)
	}
}

func TestBackgroundOutputIsBoundedAndReportsGap(t *testing.T) {
	server := processServer(t)
	info := spawnProcess(t, server, types.Command{Args: []string{"sh", "-c", "head -c 1048576 /dev/zero"}})
	_, err := AttachProcess(t.Context(), processConnection(t, server), info.ID, types.ProcessOutputOptions{}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	result, err := ProcessLogs(t.Context(), processConnection(t, server), info.ID, types.ProcessOutputOptions{}, &output, io.Discard)
	if err != nil || !result.Truncated || output.Len() == 0 || output.Len() > maxProcessOutputBytes || bytes.Count(output.Bytes(), []byte{0}) != output.Len() {
		t.Fatalf("bounded replay = %+v, bytes=%d, %v", result, output.Len(), err)
	}
	_, err = ProcessLogs(t.Context(), processConnection(t, server), info.ID, types.ProcessOutputOptions{After: result.Cursor + 1}, io.Discard, io.Discard)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "INVALID_ARGUMENT" {
		t.Fatalf("future cursor accepted: %v", err)
	}
}

func TestProcessTableLimitsAndEvictsOnlyCompletedCommands(t *testing.T) {
	manager := newProcessManager()
	now := time.Now()
	for i := range maxRetainedProcesses {
		id := fmt.Sprintf("%032x", i)
		exited := now.Add(time.Duration(i) * time.Second)
		manager.jobs[id] = &managedProcess{info: types.ProcessInfo{ID: id, ExitedAt: &exited}, changed: make(chan struct{}), exited: true}
	}
	if err := manager.admit(); err != nil || len(manager.jobs) != maxRetainedProcesses-1 {
		t.Fatalf("completed admission = %d, %v", len(manager.jobs), err)
	}
	if _, exists := manager.jobs[fmt.Sprintf("%032x", 0)]; exists {
		t.Fatal("oldest completed command was not evicted")
	}
	manager.jobs = make(map[string]*managedProcess)
	for i := range maxRunningProcesses {
		manager.jobs[string(rune(i))] = &managedProcess{info: types.ProcessInfo{Running: true}}
	}
	if err := manager.admit(); !errors.Is(err, errProcessLimit) {
		t.Fatalf("running admission = %v", err)
	}
}

func TestGuestCommandWorkingDirectoryAndUserFailClosed(t *testing.T) {
	server := processServer(t)
	directory := t.TempDir()
	file := filepath.Join(directory, "relative")
	if err := os.WriteFile(file, []byte("cwd works"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	code, err := Run(t.Context(), processConnection(t, server), types.Command{Args: []string{"cat", "relative"}, Cwd: directory}, nil, &output, io.Discard)
	if err != nil || code != 0 || output.String() != "cwd works" {
		t.Fatalf("cwd execution = %d, %q, %v", code, output.String(), err)
	}
	_, err = Run(t.Context(), processConnection(t, server), types.Command{Args: []string{"true"}, User: "kumabox-user-that-does-not-exist"}, nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("unknown guest user silently inherited the agent identity")
	}
	if runtime.GOOS == "linux" {
		info := spawnProcess(t, server, types.Command{Args: []string{"cat", "relative"}, Cwd: directory})
		output.Reset()
		_, err := AttachProcess(t.Context(), processConnection(t, server), info.ID, types.ProcessOutputOptions{}, &output, io.Discard)
		if err != nil || output.String() != "cwd works" {
			t.Fatalf("background cwd = %q, %v", output.String(), err)
		}
	}
}

func TestForegroundDisconnectAfterStdinCloseCancelsCommand(t *testing.T) {
	server := processServer(t)
	connection := processConnection(t, server)
	encoder := NewEncoder(connection)
	if err := encoder.Encode(Message{Type: MessageExec, Argv: []string{"sleep", "30"}}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(Message{Type: MessageStdinClose}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDecoder(connection).Decode(); err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	// processConnection's cleanup waits for the handler and fails if ownership leaks.
}

func TestProcessRequestCancellationUnblocksObserver(t *testing.T) {
	server := processServer(t)
	info := spawnProcess(t, server, types.Command{Args: []string{"sleep", "30"}})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := AttachProcess(ctx, processConnection(t, server), info.ID, types.ProcessOutputOptions{}, io.Discard, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("attach cancellation = %v", err)
	}
	records, err := Processes(t.Context(), processConnection(t, server))
	if err != nil || len(records) != 1 || !records[0].Running {
		t.Fatalf("canceled observer killed process: %+v, %v", records, err)
	}
}

func TestProcessOutputSupportsIndependentConcurrentObservers(t *testing.T) {
	server := processServer(t)
	release := filepath.Join(t.TempDir(), "release")
	info := spawnProcess(t, server, types.Command{Args: []string{"sh", "-c", `printf ready; while [ ! -e "$1" ]; do sleep 0.01; done; printf done`, "--", release}})
	var decoders []*Decoder
	for range 4 {
		connection := processConnection(t, server)
		if err := NewEncoder(connection).Encode(Message{Type: MessageAttach, ProcessID: info.ID}); err != nil {
			t.Fatal(err)
		}
		decoder := NewDecoder(connection)
		message, err := decoder.Decode()
		if err != nil || message.Type != MessageStdout || string(message.Data) != "ready" {
			t.Fatalf("observer replay = %+v, %v", message, err)
		}
		decoders = append(decoders, decoder)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, decoder := range decoders {
		var output bytes.Buffer
		for {
			message, err := decoder.Decode()
			if err != nil {
				t.Fatal(err)
			}
			if message.Type == MessageStdout {
				output.Write(message.Data)
			}
			if message.Type == MessageDone {
				if message.Process == nil || message.Process.ExitCode == nil || *message.Process.ExitCode != 0 || output.String() != "done" {
					t.Fatalf("observer completion = %+v, %q", message, output.String())
				}
				break
			}
		}
	}
}

func TestExtendedExecutionRejectsOldAgentWithoutFallback(t *testing.T) {
	client, guest := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = guest.Close() })
	seen := make(chan Message, 1)
	go func() {
		request, _ := NewDecoder(guest).Decode()
		seen <- request
		_ = NewEncoder(guest).sendError("unsupported operation")
	}()
	_, err := Run(t.Context(), client, types.Command{Args: []string{"true"}, User: "worker"}, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unsupported operation") {
		t.Fatalf("old agent response = %v", err)
	}
	if request := <-seen; request.Type != MessageExecV2 || request.User != "worker" {
		t.Fatalf("unsafe legacy execution frame = %+v", request)
	}
}
