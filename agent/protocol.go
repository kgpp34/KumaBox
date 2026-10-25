// Package agent implements KumaBox's host/guest command channel. The wire
// format carries one bounded JSON message per line and one operation per
// vsock connection.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/kumabox/kumabox/types"
)

const (
	// Port is the guest endpoint for host-initiated agent sessions.
	Port uint32 = 1024

	// MessageExec starts one command session.
	MessageExec = "exec"
	// MessageExecV2 prevents old agents silently ignoring cwd or user selection.
	MessageExecV2 = "exec_v2"
	// Background commands are owned by the agent rather than their connection.
	MessageSpawn     = "spawn"
	MessageProcesses = "processes"
	MessageProcess   = "process"
	MessageSignal    = "signal"
	MessageLogs      = "process_logs"
	MessageAttach    = "process_attach"
	MessageGap       = "output_gap"
	MessageDone      = "done"
	// Guest filesystem requests use streamed binary data and metadata frames.
	MessageFileRead    = "file_read"
	MessageFileWrite   = "file_write"
	MessageFileList    = "file_list"
	MessageFileStat    = "file_stat"
	MessageFileMkdir   = "file_mkdir"
	MessageFileRemove  = "file_remove"
	MessageFileRename  = "file_rename"
	MessageFileInfo    = "file_info"
	MessageFileEntries = "file_entries"
	MessageFileData    = "file_data"
	MessageFileEnd     = "file_end"
	MessageFileReady   = "file_ready"
	// Port forwarding carries TCP bytes and independent directional EOFs.
	MessagePortForward = "port_forward"
	MessagePortReady   = "port_ready"
	MessagePortData    = "port_data"
	MessagePortEnd     = "port_end"
	// MessageReseed mixes fresh host entropy into the guest random pool.
	MessageReseed = "reseed"
	// MessageConfigureNetwork applies host-assigned guest identity and NIC settings.
	MessageConfigureNetwork = "configure_network"
	// MessageConfigureNetworkLinks additionally applies named-link MAC changes.
	// Older agents reject it explicitly, allowing a deliberate exec fallback.
	MessageConfigureNetworkLinks = "configure_network_links"
	// MessageStdin carries one command input chunk.
	MessageStdin = "stdin"
	// MessageStdinClose closes command input without ending the session.
	MessageStdinClose = "stdin_close"
	// MessageStarted reports the guest process identifier.
	MessageStarted = "started"
	// MessageStdout carries one standard-output chunk.
	MessageStdout = "stdout"
	// MessageStderr carries one standard-error chunk.
	MessageStderr = "stderr"
	// MessageExit terminates a protocol session with a command status.
	MessageExit = "exit"
	// MessageError terminates a failed protocol session with a diagnostic.
	MessageError = "error"

	initialFrameBuffer = 64 * 1024
	maximumFrameSize   = 8 * 1024 * 1024
	streamChunkSize    = 32 * 1024
	fileListBatchSize  = 128
)

var errTerminalMessageSent = errors.New("terminal agent message already sent")

// Message is the protocol union carried by each NDJSON frame. Fields
// unrelated to Type remain empty and are omitted from the wire representation.
type Message struct {
	// Port is a guest IPv4 loopback TCP port, never a host or arbitrary address.
	Port uint32 `json:"port,omitempty"`
	// Path and Destination refer exclusively to paths inside the guest.
	Path        string           `json:"path,omitempty"`
	Destination string           `json:"destination,omitempty"`
	Mode        *uint32          `json:"mode,omitempty"`
	Parents     bool             `json:"parents,omitempty"`
	Recursive   bool             `json:"recursive,omitempty"`
	File        *types.FileInfo  `json:"file,omitempty"`
	Entries     []types.FileInfo `json:"entries,omitempty"`
	// Type selects the fields and transition represented by this message.
	Type string `json:"type"`
	// Argv contains the executable and arguments for MessageExec.
	Argv []string `json:"argv,omitempty"`
	// Env contains environment overrides for MessageExec.
	Env  map[string]string `json:"env,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
	User string            `json:"user,omitempty"`
	// ProcessID is an opaque agent-owned handle, distinct from the OS PID.
	ProcessID string              `json:"process_id,omitempty"`
	Signal    types.ProcessSignal `json:"signal,omitempty"`
	Process   *types.ProcessInfo  `json:"process,omitempty"`
	// Cursor identifies an output frame; requests resume strictly after it.
	Cursor uint64 `json:"cursor,omitempty"`
	// Code classifies new operation failures without parsing diagnostic text.
	Code string `json:"code,omitempty"`
	// Data carries stdin, stdout, stderr, or entropy bytes.
	Data []byte `json:"data,omitempty"`
	// PID identifies a process reported by MessageStarted.
	PID int `json:"pid,omitempty"`
	// ExitCode is the guest command status reported by MessageExit.
	ExitCode int `json:"exit_code,omitempty"`
	// Message contains the diagnostic reported by MessageError.
	Message string `json:"message,omitempty"`
	// RegenMachineID requests a new persistent machine identity during reseed.
	RegenMachineID bool `json:"regen_machine_id,omitempty"`
	// Network carries the complete guest network configuration for one update.
	Network *NetworkConfig `json:"network,omitempty"`
}

// Decoder reads bounded newline-delimited JSON messages.
type Decoder struct {
	scanner *bufio.Scanner
}

// NewDecoder constructs a decoder whose frame limit prevents an untrusted
// peer from growing memory without bound.
func NewDecoder(reader io.Reader) *Decoder {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, initialFrameBuffer), maximumFrameSize)
	return &Decoder{scanner: scanner}
}

// Decode reads one complete protocol message.
func (d *Decoder) Decode() (Message, error) {
	if !d.scanner.Scan() {
		if err := d.scanner.Err(); err != nil {
			return Message{}, fmt.Errorf("read agent frame: %w", err)
		}
		return Message{}, io.EOF
	}
	var message Message
	if err := json.Unmarshal(d.scanner.Bytes(), &message); err != nil {
		return Message{}, fmt.Errorf("decode agent frame: %w", err)
	}
	return message, nil
}

// Encoder serializes concurrent stdout and stderr writers onto one stream.
// Exit and error are terminal: no later frame may be emitted.
type Encoder struct {
	mu       sync.Mutex
	encoder  *json.Encoder
	terminal bool
}

// NewEncoder constructs a newline-delimited JSON encoder.
func NewEncoder(writer io.Writer) *Encoder {
	return &Encoder{encoder: json.NewEncoder(writer)}
}

// Encode writes one message atomically with respect to other writers.
func (e *Encoder) Encode(message Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.terminal {
		return errTerminalMessageSent
	}
	if err := e.encoder.Encode(message); err != nil {
		return fmt.Errorf("write agent frame: %w", err)
	}
	if terminalMessage(message.Type) {
		e.terminal = true
	}
	return nil
}

func (e *Encoder) sendError(format string, args ...any) error {
	return e.Encode(Message{Type: MessageError, Message: fmt.Sprintf(format, args...)})
}

func terminalMessage(messageType string) bool {
	return messageType == MessageExit || messageType == MessageError || messageType == MessageDone
}

// framedWriter converts process output writes into bounded protocol frames.
type framedWriter struct {
	messageType string
	encoder     *Encoder
	cancel      context.CancelFunc
	lastError   atomic.Pointer[error]
}

func (w *framedWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		length := min(len(data), streamChunkSize)
		if err := w.encoder.Encode(Message{Type: w.messageType, Data: data[:length]}); err != nil {
			errorCopy := err
			if w.lastError.CompareAndSwap(nil, &errorCopy) {
				w.cancel()
			}
			return written, err
		}
		written += length
		data = data[length:]
	}
	return written, nil
}

func (w *framedWriter) err() error {
	if err := w.lastError.Load(); err != nil {
		return *err
	}
	return nil
}

// RemoteError preserves structured guest failures for application adapters.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string { return "guest agent: " + e.Message }

// exchange owns one bounded request/stream exchange. Cancellation closes
// the transport, unblocking both a slow response read and a blocked request write.
func exchange(ctx context.Context, connection io.ReadWriteCloser, request Message, receive func(Message) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	defer connection.Close() //nolint:errcheck // one operation owns the transport
	if err := NewEncoder(connection).Encode(request); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("send agent request: %w", err)
	}
	decoder := NewDecoder(connection)
	for {
		message, err := decoder.Decode()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read agent response: %w", err)
		}
		if message.Type == MessageError {
			return &RemoteError{Code: message.Code, Message: message.Message}
		}
		if err := receive(message); err != nil {
			return err
		}
		if message.Type == MessageDone {
			return nil
		}
	}
}

// contextIOError preserves caller cancellation when closing a transport unblocks I/O.
func contextIOError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
