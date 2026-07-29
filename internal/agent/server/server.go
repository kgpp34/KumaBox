// Package server implements the guest-side KumaBox agent service.
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

const (
	Version = "0.3.0"
	Port    = protocol.AgentPort
)

var capabilities = []string{
	string(protocol.CapabilityPingPong),
	string(protocol.CapabilityExec),
	string(protocol.CapabilityExecStream),
	string(protocol.CapabilityExecTTY),
	string(protocol.CapabilityIdentity),
}

const streamChunkSize = 32 * 1024

type pingRequest struct {
	Type protocol.RequestType `json:"type"`
}

type pingResponse struct {
	OK           bool     `json:"ok"`
	Version      string   `json:"version,omitempty"`
	OS           string   `json:"os,omitempty"`
	Hostname     string   `json:"hostname,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type execRequest struct {
	Type    protocol.RequestType `json:"type"`
	Args    []string             `json:"args"`
	Env     []string             `json:"env,omitempty"`
	WorkDir string               `json:"workdir,omitempty"`
	Stdin   []byte               `json:"stdin,omitempty"`
}

type execResponse struct {
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exitCode"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

type identityRequest struct {
	Type       protocol.RequestType `json:"type"`
	Hostname   string               `json:"hostname"`
	Interfaces []interfaceIdentity  `json:"interfaces,omitempty"`
}

type interfaceIdentity struct {
	Name    string   `json:"name"`
	MAC     string   `json:"mac"`
	IP      string   `json:"ip,omitempty"`
	Prefix  int      `json:"prefix,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

type identityResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

var configureIdentity = applyIdentity

func Serve() error {
	return serveVsock(Port, handleConn)
}

func handleConn(rw io.ReadWriter) {
	reader := bufio.NewReader(rw)
	line, err := reader.ReadString('\n')
	if err != nil {
		writeResponse(rw, pingResponse{OK: false, Error: err.Error()})
		return
	}
	var envelope struct {
		Version string `json:"version"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err == nil && envelope.Version == protocol.VersionV1 {
		var first protocol.Frame
		if err := json.Unmarshal([]byte(line), &first); err != nil {
			writeStreamError(rw, "unknown", protocol.ErrorInvalidFrame, "invalid JSON frame")
			return
		}
		if err := first.Validate(); err != nil {
			writeStreamError(rw, first.ID, protocol.ErrorInvalidFrame, err.Error())
			return
		}
		if first.Type != protocol.FrameExec {
			writeStreamError(rw, first.ID, protocol.ErrorInvalidRequest, "first stream frame must be exec")
			return
		}
		handleStreamExec(reader, rw, first)
		return
	}
	var req pingRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		writeResponse(rw, pingResponse{OK: false, Error: "invalid request"})
		return
	}
	switch protocol.RequestType(strings.ToLower(string(req.Type))) {
	case protocol.RequestPing:
		handlePingPong(rw)
	case protocol.RequestExec:
		handleExec(rw, []byte(line))
	case protocol.RequestIdentity:
		handleIdentity(rw, []byte(line))
	default:
		writeResponse(rw, pingResponse{OK: false, Error: "unsupported request"})
	}
}

func handleStreamExec(reader *bufio.Reader, rw io.ReadWriter, request protocol.Frame) {
	if request.TTY {
		handleTTYExec(reader, rw, request)
		return
	}
	if request.User != "" {
		writeStreamError(rw, request.ID, protocol.ErrorCapabilityMissing, "user selection is not supported by this protocol handler")
		return
	}

	cmd := exec.Command(request.Args[0], request.Args[1:]...) //nolint:gosec
	cmd.Dir = request.WorkDir
	cmd.Env = append(os.Environ(), environmentPairs(request.Env)...)
	writer := &streamWriter{writer: rw}
	cmd.Stdout = &streamOutputWriter{writer: writer, id: request.ID, frameType: protocol.FrameStdout, stream: protocol.StreamStdout}
	cmd.Stderr = &streamOutputWriter{writer: writer, id: request.ID, frameType: protocol.FrameStderr, stream: protocol.StreamStderr}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorExecFailed, err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorExecFailed, err.Error())
		return
	}

	if err := writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameReady, ID: request.ID}); err != nil {
		_ = cmd.Process.Kill()
		return
	}

	decoder := protocol.NewDecoder(reader)
	inputClosed := false
	for !inputClosed {
		frame, readErr := decoder.ReadFrame()
		if readErr != nil {
			_ = cmd.Process.Kill()
			return
		}
		if frame.ID != request.ID {
			_ = writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameError, ID: request.ID, Code: protocol.ErrorInvalidFrame, Message: "stdin frame has unexpected exec id"})
			_ = cmd.Process.Kill()
			return
		}
		switch frame.Type {
		case protocol.FrameStdin:
			if _, writeErr := stdin.Write(frame.Data); writeErr != nil {
				_ = cmd.Process.Kill()
				return
			}
			if frame.End {
				_ = stdin.Close()
				inputClosed = true
			}
		default:
			_ = writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameError, ID: request.ID, Code: protocol.ErrorInvalidFrame, Message: "non-stdin frame received before stdin ended"})
			_ = cmd.Process.Kill()
			return
		}
	}

	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		exitCode = commandExitCode(waitErr)
	}
	_ = writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameExit, ID: request.ID, ExitCode: exitCode})
}

func environmentPairs(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

func commandExitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		return 128
	}
	return 127
}

type streamWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *streamWriter) Write(frame protocol.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return protocol.WriteFrame(w.writer, frame)
}

type streamOutputWriter struct {
	writer    *streamWriter
	id        string
	frameType protocol.FrameType
	stream    protocol.Stream
}

func (w *streamOutputWriter) Write(data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		chunkSize := min(len(data), streamChunkSize)
		chunk := append([]byte(nil), data[:chunkSize]...)
		if err := w.writer.Write(protocol.Frame{
			Version: protocol.VersionV1,
			Type:    w.frameType,
			ID:      w.id,
			Stream:  w.stream,
			Data:    chunk,
		}); err != nil {
			return total, err
		}
		total += chunkSize
		data = data[chunkSize:]
	}
	return total, nil
}

func writeStreamError(w io.Writer, id string, code protocol.ErrorCode, message string) {
	if id == "" {
		id = "unknown"
	}
	_ = protocol.WriteFrame(w, protocol.Frame{
		Version: protocol.VersionV1,
		Type:    protocol.FrameError,
		ID:      id,
		Code:    code,
		Message: message,
	})
}

func handleIdentity(w io.Writer, raw []byte) {
	var req identityRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeResponse(w, identityResponse{OK: false, Error: "invalid identity request"})
		return
	}
	if err := configureIdentity(req); err != nil {
		writeResponse(w, identityResponse{OK: false, Error: err.Error()})
		return
	}
	writeResponse(w, identityResponse{OK: true})
}

func handlePingPong(w io.Writer) {
	hostname, _ := os.Hostname()
	writeResponse(w, pingResponse{
		OK:           true,
		Version:      Version,
		OS:           runtime.GOOS,
		Hostname:     hostname,
		Capabilities: append([]string(nil), capabilities...),
	})
}

func handleExec(w io.Writer, raw []byte) {
	var req execRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeResponse(w, execResponse{OK: false, ExitCode: 127, Error: "invalid exec request"})
		return
	}
	if len(req.Args) == 0 || req.Args[0] == "" {
		writeResponse(w, execResponse{OK: false, ExitCode: 127, Error: "exec args must not be empty"})
		return
	}

	cmd := exec.Command(req.Args[0], req.Args[1:]...) //nolint:gosec
	cmd.Dir = req.WorkDir
	cmd.Env = append(os.Environ(), req.Env...)
	cmd.Stdin = bytes.NewReader(req.Stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	resp := execResponse{OK: true}
	if err := cmd.Run(); err != nil {
		resp.OK = false
		resp.Error = err.Error()
		if exitErr, ok := err.(*exec.ExitError); ok {
			resp.ExitCode = exitErr.ExitCode()
		} else {
			resp.ExitCode = 127
		}
	}
	resp.Stdout = stdout.Bytes()
	resp.Stderr = stderr.Bytes()
	writeResponse(w, resp)
}

func writeResponse(w io.Writer, resp any) {
	raw, err := json.Marshal(resp)
	if err != nil {
		_, _ = fmt.Fprintln(w, `{"ok":false,"error":"encode response"}`)
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}
