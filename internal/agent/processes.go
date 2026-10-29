package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/kumabox/kumabox/internal/types"
)

const (
	maxRunningProcesses     = 64
	maxRetainedProcesses    = 128
	maxProcessOutputBytes   = 256 << 10
	maxProcessOutputFrames  = 256
	maxDetachedCommandBytes = 64 << 10
)

var (
	errProcessNotFound = errors.New("agent process not found or already evicted")
	errProcessLimit    = errors.New("agent background process limit reached")
	errProcessExited   = errors.New("agent process has already exited")
)

// processManager owns detached commands independently of transport sessions.
// It retains a bounded set of completed records, evicting the oldest completed
// command on admission. Running commands are never evicted to make space.
//
// spawn -> running -> exited -> retained -> evicted
//
//	|                   |
//	+-- output ring ----+--> logs / attach (independent observers)
type processManager struct {
	mu     sync.Mutex
	jobs   map[string]*managedProcess
	closed bool
	wait   sync.WaitGroup
}

type managedProcess struct {
	command *exec.Cmd
	// signalMu fences group signalling against reaping the group leader.
	signalMu sync.Mutex
	exited   bool
	// mu protects metadata and the output ring; it is never held during I/O.
	mu      sync.Mutex
	info    types.ProcessInfo
	frames  []Message
	bytes   int
	cursor  uint64
	changed chan struct{}
}

func newProcessManager() *processManager {
	return &processManager{jobs: make(map[string]*managedProcess)}
}

func (m *processManager) spawn(request Message) (types.ProcessInfo, error) {
	if err := request.command().Validate(); err != nil {
		return types.ProcessInfo{}, err
	}
	size := len(request.Cwd) + len(request.User)
	for _, argument := range request.Argv {
		size += len(argument)
	}
	for key, value := range request.Env {
		size += len(key) + len(value)
	}
	if size > maxDetachedCommandBytes {
		return types.ProcessInfo{}, errors.New("background command exceeds 64 KiB")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return types.ProcessInfo{}, err
	}
	command := exec.Command(request.Argv[0], request.Argv[1:]...) //nolint:gosec // explicit owner-controlled argv, never an implicit shell
	if err := prepareCommand(command, request); err != nil {
		return types.ProcessInfo{}, err
	}
	// Detached lifetime is owned by close/signal, not CommandContext or a socket.
	command.Cancel = nil
	job := &managedProcess{
		command: command, changed: make(chan struct{}),
		info: types.ProcessInfo{ID: hex.EncodeToString(nonce[:]), Args: slices.Clone(request.Argv), Cwd: request.Cwd, User: request.User, Running: true, StartedAt: time.Now().UTC()},
	}
	command.Stdout = &processOutputWriter{job: job, kind: MessageStdout}
	command.Stderr = &processOutputWriter{job: job, kind: MessageStderr}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return types.ProcessInfo{}, errors.New("guest agent is shutting down")
	}
	if err := m.admit(); err != nil {
		return types.ProcessInfo{}, err
	}
	if err := command.Start(); err != nil {
		return types.ProcessInfo{}, fmt.Errorf("start background command: %w", err)
	}
	job.mu.Lock()
	job.info.PID = command.Process.Pid
	job.mu.Unlock()
	m.jobs[job.info.ID] = job
	m.wait.Add(1)
	go func() {
		defer m.wait.Done()
		job.finish(waitManagedProcess(command, &job.signalMu, &job.exited))
	}()
	return job.inspect(), nil
}

// admit runs under the table lock. Completed metadata has a fixed retention
// count rather than a timer so no maintenance goroutine is needed in the guest.
func (m *processManager) admit() error {
	running := 0
	var oldestID string
	var oldestExit time.Time
	for id, job := range m.jobs {
		job.mu.Lock()
		active, exitedAt := job.info.Running, job.info.ExitedAt
		job.mu.Unlock()
		if active {
			running++
		} else if oldestID == "" || exitedAt.Before(oldestExit) || (exitedAt.Equal(oldestExit) && id < oldestID) {
			oldestID, oldestExit = id, *exitedAt
		}
	}
	if running >= maxRunningProcesses {
		return errProcessLimit
	}
	if len(m.jobs) >= maxRetainedProcesses {
		if oldestID == "" {
			return errProcessLimit
		}
		delete(m.jobs, oldestID)
	}
	return nil
}

func (m *processManager) lookup(id string) (*managedProcess, error) {
	if err := types.ValidateProcessID(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return nil, errProcessNotFound
	}
	return job, nil
}

func (m *processManager) list() []types.ProcessInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]types.ProcessInfo, 0, len(m.jobs))
	for _, job := range m.jobs {
		result = append(result, job.inspect())
	}
	slices.SortFunc(result, func(a, b types.ProcessInfo) int { return a.StartedAt.Compare(b.StartedAt) })
	return result
}

func (m *processManager) close() {
	m.mu.Lock()
	m.closed = true
	jobs := make([]*managedProcess, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	for _, job := range jobs {
		_ = job.signal(types.ProcessSignalKILL)
	}
	m.wait.Wait()
}

func (p *managedProcess) inspect() types.ProcessInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := p.info
	info.Args = slices.Clone(info.Args)
	return info
}

func (p *managedProcess) signal(signal types.ProcessSignal) error {
	if err := signal.Validate(); err != nil {
		return err
	}
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if p.exited {
		return errProcessExited
	}
	if err := signalManagedProcess(p.command, signal); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return errProcessExited
		}
		return err
	}
	return nil
}

func (p *managedProcess) finish(err error) {
	code := 0
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		code = processExitCode(exitError.ProcessState)
	} else if err != nil {
		code = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.info.Running = false
	p.info.ExitCode = &code
	now := time.Now().UTC()
	p.info.ExitedAt = &now
	if err != nil && !errors.As(err, &exitError) {
		p.info.Error = err.Error()
		p.info.OutputTruncated = true
	}
	p.notify()
}

func (p *managedProcess) notify() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// output returns a consistent replay batch plus a notification channel armed
// under the same lock, eliminating a replay/live-subscription race. Slow readers
// do not block a command: a cursor gap explicitly reports overwritten output.
func (p *managedProcess) output(after uint64) ([]Message, types.ProcessInfo, uint64, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	frames := make([]Message, 0, len(p.frames))
	for _, frame := range p.frames {
		if frame.Cursor > after {
			frames = append(frames, frame)
		}
	}
	return frames, p.info, p.cursor, p.changed
}

type processOutputWriter struct {
	job  *managedProcess
	kind string
}

func (w *processOutputWriter) Write(data []byte) (int, error) {
	total := len(data)
	for len(data) > 0 {
		length := min(len(data), streamChunkSize)
		w.job.append(w.kind, data[:length])
		data = data[length:]
	}
	return total, nil
}

func (p *managedProcess) append(kind string, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cursor++
	p.frames = append(p.frames, Message{Type: kind, Data: slices.Clone(data), Cursor: p.cursor})
	p.bytes += len(data)
	for p.bytes > maxProcessOutputBytes || len(p.frames) > maxProcessOutputFrames {
		p.bytes -= len(p.frames[0].Data)
		p.frames[0] = Message{}
		p.frames = p.frames[1:]
		p.info.OutputTruncated = true
	}
	p.notify()
}
