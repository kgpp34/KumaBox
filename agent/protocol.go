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
)

const (
	// Port is the guest endpoint for host-initiated agent sessions.
	Port uint32 = 1024

	// MessageExec starts one command session.
	MessageExec = "exec"
	// MessageReseed reserves the identity refresh operation.
	MessageReseed = "reseed"
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
)

var errTerminalMessageSent = errors.New("terminal agent message already sent")

// Message is the protocol union carried by each NDJSON frame. Fields
// unrelated to Type remain empty and are omitted from the wire representation.
type Message struct {
	// Type selects the fields and transition represented by this message.
	Type string `json:"type"`
	// Argv contains the executable and arguments for MessageExec.
	Argv []string `json:"argv,omitempty"`
	// Env contains environment overrides for MessageExec.
	Env map[string]string `json:"env,omitempty"`
	// Data carries stdin, stdout, stderr, or entropy bytes.
	Data []byte `json:"data,omitempty"`
	// PID identifies a process reported by MessageStarted.
	PID int `json:"pid,omitempty"`
	// ExitCode is the guest command status reported by MessageExit.
	ExitCode int `json:"exit_code,omitempty"`
	// Message contains the diagnostic reported by MessageError.
	Message string `json:"message,omitempty"`
	// RegenMachineID requests machine identity renewal during a future reseed.
	RegenMachineID bool `json:"regen_machine_id,omitempty"`
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
	return messageType == MessageExit || messageType == MessageError
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
