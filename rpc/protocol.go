// Package rpc implements the local node protocol over HTTP/2 and Unix sockets.
// Each request has bounded JSON metadata followed by an optional input stream.
// Responses carry progress, independent output streams, and one terminal result.
package rpc

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/kumabox/kumabox/errdefs"
)

const (
	// Prefix versions the local protocol independently of public REST routes.
	Prefix      = "/rpc/v1/"
	maxMetadata = 1 << 20
	maxFrame    = 8 << 20
	chunkSize   = 32 << 10
)

// Request separates operation parameters from the following raw input stream.
type Request struct {
	Params   json.RawMessage `json:"params"`
	Deadline time.Time       `json:"deadline,omitzero"`
}

// Frame carries one response event. Result is also present on partial failure.
type Frame struct {
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data,omitempty"`
	Bytes   []byte          `json:"bytes,omitempty"`
	Failure *Failure        `json:"failure,omitempty"`
}

// Failure preserves the handling code, commit boundary and complete diagnostic.
type Failure struct {
	Message   string        `json:"message"`
	Class     errdefs.Class `json:"class"`
	Code      errdefs.Code  `json:"code"`
	Committed bool          `json:"committed"`
	Canceled  bool          `json:"canceled,omitempty"`
	Deadline  bool          `json:"deadline,omitzero"`
}

func failure(err error) *Failure {
	if err == nil {
		return nil
	}
	f := &Failure{
		Message: err.Error(), Class: errdefs.ClassInternal, Code: errdefs.CodeInternal,
		Canceled: errors.Is(err, context.Canceled), Deadline: errors.Is(err, context.DeadlineExceeded),
	}
	var classified *errdefs.Error
	if errors.As(err, &classified) {
		f.Class = classified.Class
		f.Code = classified.Code
		f.Committed = classified.Committed
	}
	return f
}

// Error reconstructs a classified error without repeating its rendered code.
func (f *Failure) Error() string { return f.Message }

// Unwrap preserves domain exit codes and cancellation checks at the client.
func (f *Failure) Unwrap() error {
	var cause error
	switch {
	case f.Canceled:
		cause = context.Canceled
	case f.Deadline:
		cause = context.DeadlineExceeded
	}
	return &errdefs.Error{Class: f.Class, Code: f.Code, Committed: f.Committed, Cause: cause}
}

// ReadFrame enforces a size bound before decoding untrusted response data.
func ReadFrame(scanner *bufio.Scanner) (Frame, error) {
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return Frame{}, err
		}
		return Frame{}, io.EOF
	}
	var frame Frame
	err := json.Unmarshal(scanner.Bytes(), &frame)
	return frame, err
}

// Scanner creates the bounded response decoder shared by clients and tests.
func Scanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxFrame)
	return scanner
}

// EncodeRequest prefixes metadata with its byte length; input remains streamed.
func EncodeRequest(params any, deadline time.Time) ([]byte, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	data, err = json.Marshal(Request{Params: data, Deadline: deadline})
	if err != nil {
		return nil, err
	}
	if len(data) > maxMetadata {
		return nil, errors.New("node request metadata exceeds 1 MiB")
	}
	header := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(header, uint32(len(data))) //nolint:gosec // length is capped at 1 MiB above, within uint32
	copy(header[4:], data)
	return header, nil
}

// Session serializes output from concurrent workflow reporters and I/O writers.
// Input is closed by net/http when the request is canceled.
type Session struct {
	Input  io.ReadCloser
	writer http.ResponseWriter
	mu     sync.Mutex
	err    error
}

// Emit sends one bounded event and immediately flushes it to the client.
func (s *Session) Emit(kind string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.send(Frame{Type: kind, Data: data})
}

func (s *Session) send(frame Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	data, err := json.Marshal(frame)
	if err == nil && len(data) >= maxFrame {
		err = errors.New("node response frame exceeds 8 MiB")
	}
	if err == nil {
		controller := http.NewResponseController(s.writer)
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
		_, err = s.writer.Write(append(data, '\n'))
	}
	if err == nil {
		err = http.NewResponseController(s.writer).Flush()
	}
	s.err = err
	return err
}

// Output returns a chunked writer for stdout or stderr, with bounded memory use.
func (s *Session) Output(kind string) io.Writer { return streamWriter{session: s, kind: kind} }

type streamWriter struct {
	session *Session
	kind    string
}

func (w streamWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		n := min(len(data), chunkSize)
		if err := w.session.send(Frame{Type: w.kind, Bytes: data[:n]}); err != nil {
			return written, err
		}
		written += n
		data = data[n:]
	}
	return written, nil
}

// Register binds a typed request once during server construction.
func Register[T any](mux *http.ServeMux, method string, run func(context.Context, *Session, T) (any, error)) {
	mux.HandleFunc("POST "+Prefix+method, func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			http.Error(w, "local node protocol requires HTTP/2", http.StatusHTTPVersionNotSupported)
			return
		}
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			http.Error(w, "request deadline unavailable", 500)
			return
		}
		var size [4]byte
		if _, err := io.ReadFull(r.Body, size[:]); err != nil {
			http.Error(w, "missing request metadata", 400)
			return
		}
		length := binary.BigEndian.Uint32(size[:])
		if length == 0 || length > maxMetadata {
			http.Error(w, "invalid request metadata length", 400)
			return
		}
		data := make([]byte, int(length))
		if _, err := io.ReadFull(r.Body, data); err != nil {
			http.Error(w, "incomplete request metadata", 400)
			return
		}
		var request Request
		if err := json.Unmarshal(data, &request); err != nil {
			http.Error(w, "invalid request envelope", 400)
			return
		}
		var params T
		if err := json.Unmarshal(request.Params, &params); err != nil {
			http.Error(w, "invalid operation parameters", 400)
			return
		}
		_ = controller.SetReadDeadline(time.Time{})
		ctx := r.Context()
		cancel := func() {}
		if !request.Deadline.IsZero() {
			ctx, cancel = context.WithDeadline(ctx, request.Deadline)
		}
		defer cancel()
		w.Header().Set("Content-Type", "application/x-ndjson")
		session := &Session{Input: r.Body, writer: w}
		result, err := run(ctx, session, params)
		encoded, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			err = errors.Join(err, fmt.Errorf("encode result: %w", encodeErr))
		}
		_ = session.send(Frame{Type: "result", Data: encoded, Failure: failure(err)})
	})
}

// SetWriteDeadline bounds a stalled guest-port output without timing out idle streams.
func (s *Session) SetWriteDeadline(deadline time.Time) error {
	return http.NewResponseController(s.writer).SetWriteDeadline(deadline)
}
