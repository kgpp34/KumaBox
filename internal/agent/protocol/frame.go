package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

const (
	// VersionV1 is the first version of the streaming agent wire protocol.
	VersionV1 = "kumabox.agent.v1"

	// MaxFrameBytes bounds one JSON frame. Stream data must be chunked by the
	// sender instead of allowing a single unbounded allocation.
	MaxFrameBytes = 1 << 20
)

// FrameType identifies a message in a streaming agent session.
type FrameType string

const (
	FramePing   FrameType = "ping"
	FrameExec   FrameType = "exec"
	FrameStdin  FrameType = "stdin"
	FrameStdout FrameType = "stdout"
	FrameStderr FrameType = "stderr"
	FrameResize FrameType = "resize"
	FrameSignal FrameType = "signal"
	FrameExit   FrameType = "exit"
	FrameError  FrameType = "error"
	FrameReady  FrameType = "ready"
)

// Stream identifies the direction or logical stream carrying frame data.
type Stream string

const (
	StreamStdin  Stream = "stdin"
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// ErrorCode is a stable machine-readable protocol error.
type ErrorCode string

func (e ErrorCode) Error() string { return string(e) }

const (
	ErrorInvalidFrame       ErrorCode = "INVALID_FRAME"
	ErrorUnsupportedVersion ErrorCode = "UNSUPPORTED_VERSION"
	ErrorUnsupportedFrame   ErrorCode = "UNSUPPORTED_FRAME"
	ErrorInvalidRequest     ErrorCode = "INVALID_REQUEST"
	ErrorExecFailed         ErrorCode = "EXEC_FAILED"
	ErrorAgentUnavailable   ErrorCode = "AGENT_UNAVAILABLE"
	ErrorCapabilityMissing  ErrorCode = "CAPABILITY_MISSING"
)

// Frame is the typed wire envelope for a streaming agent session. Fields are
// intentionally concrete so callers do not need to pass unvalidated JSON
// fragments between the host and guest.
type Frame struct {
	Version string    `json:"version"`
	Type    FrameType `json:"type"`
	ID      string    `json:"id,omitempty"`

	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	User    string            `json:"user,omitempty"`
	TTY     bool              `json:"tty,omitempty"`

	Stream Stream `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`
	End    bool   `json:"end,omitempty"`

	Rows    uint16 `json:"rows,omitempty"`
	Columns uint16 `json:"columns,omitempty"`
	Signal  string `json:"signal,omitempty"`

	ExitCode int       `json:"exitCode,omitempty"`
	Code     ErrorCode `json:"code,omitempty"`
	Message  string    `json:"message,omitempty"`
}

// Validate checks the common envelope and the fields required by each frame.
func (f Frame) Validate() error {
	if f.Version != VersionV1 {
		return fmt.Errorf("%w: %q", ErrorUnsupportedVersion, f.Version)
	}
	if !knownFrameType(f.Type) {
		return fmt.Errorf("%w: %q", ErrorUnsupportedFrame, f.Type)
	}
	if f.Type != FramePing && f.ID == "" {
		return fmt.Errorf("%w: frame %q requires id", ErrorInvalidFrame, f.Type)
	}
	switch f.Type {
	case FrameExec:
		if len(f.Args) == 0 || f.Args[0] == "" {
			return fmt.Errorf("%w: exec args must not be empty", ErrorInvalidRequest)
		}
	case FrameStdin:
		if f.Stream != StreamStdin {
			return fmt.Errorf("%w: frame %q requires stdin stream", ErrorInvalidFrame, f.Type)
		}
	case FrameStdout:
		if f.Stream != StreamStdout {
			return fmt.Errorf("%w: frame %q requires stdout stream", ErrorInvalidFrame, f.Type)
		}
	case FrameStderr:
		if f.Stream != StreamStderr {
			return fmt.Errorf("%w: frame %q requires stderr stream", ErrorInvalidFrame, f.Type)
		}
	case FrameResize:
		if f.Rows == 0 || f.Columns == 0 {
			return fmt.Errorf("%w: resize dimensions must be non-zero", ErrorInvalidRequest)
		}
	case FrameSignal:
		if f.Signal == "" {
			return fmt.Errorf("%w: signal must not be empty", ErrorInvalidRequest)
		}
	case FrameExit:
		if f.ExitCode < 0 {
			return fmt.Errorf("%w: exit code must not be negative", ErrorInvalidFrame)
		}
	case FrameError:
		if f.Code == "" || f.Message == "" {
			return fmt.Errorf("%w: error frame requires code and message", ErrorInvalidFrame)
		}
	}
	return nil
}

// WriteFrame writes one newline-delimited JSON frame.
func WriteFrame(w io.Writer, frame Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode agent frame: %w", err)
	}
	if len(raw)+1 > MaxFrameBytes {
		return fmt.Errorf("%w: frame is %d bytes, maximum is %d", ErrorInvalidFrame, len(raw)+1, MaxFrameBytes)
	}
	raw = append(raw, '\n')
	for len(raw) > 0 {
		n, err := w.Write(raw)
		if err != nil {
			return fmt.Errorf("write agent frame: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		raw = raw[n:]
	}
	return nil
}

// Decoder reads consecutive frames from one stream without losing bytes that
// belong to the following frame.
type Decoder struct {
	reader *bufio.Reader
}

// NewDecoder creates a bounded streaming frame decoder.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{reader: bufio.NewReader(r)}
}

// ReadFrame reads and validates the next frame.
func (d *Decoder) ReadFrame() (Frame, error) {
	if d == nil || d.reader == nil {
		return Frame{}, fmt.Errorf("%w: nil decoder", ErrorInvalidFrame)
	}
	return readFrame(d.reader)
}

// ReadFrame reads one newline-delimited JSON frame and validates it before
// returning it to the caller. Use NewDecoder when reading more than one frame
// from the same stream.
func ReadFrame(r io.Reader) (Frame, error) {
	return NewDecoder(r).ReadFrame()
}

// readFrame is split out so a session can reuse one buffered reader without
// losing bytes belonging to the next frame.
func readFrame(r *bufio.Reader) (Frame, error) {
	var line bytes.Buffer
	for {
		part, err := r.ReadSlice('\n')
		line.Write(part)
		if line.Len() > MaxFrameBytes {
			return Frame{}, fmt.Errorf("%w: frame exceeds %d bytes", ErrorInvalidFrame, MaxFrameBytes)
		}
		if err == nil {
			break
		}
		if err != bufio.ErrBufferFull {
			return Frame{}, fmt.Errorf("read agent frame: %w", err)
		}
	}
	raw := line.Bytes()
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return Frame{}, fmt.Errorf("%w: frame is not newline terminated", ErrorInvalidFrame)
	}
	var frame Frame
	if err := json.Unmarshal(raw[:len(raw)-1], &frame); err != nil {
		return Frame{}, fmt.Errorf("%w: decode JSON: %v", ErrorInvalidFrame, err)
	}
	if err := frame.Validate(); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func knownFrameType(frameType FrameType) bool {
	switch frameType {
	case FramePing, FrameExec, FrameStdin, FrameStdout, FrameStderr,
		FrameResize, FrameSignal, FrameExit, FrameError, FrameReady:
		return true
	default:
		return false
	}
}
