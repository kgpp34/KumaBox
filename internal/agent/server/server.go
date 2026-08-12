// Package server implements the guest-side KumaBox agent service.
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kumabox/kumabox/internal/agent/protocol"
)

const (
	Version = "0.3.3"
	Port    = protocol.AgentPort
)

var capabilities = []string{
	string(protocol.CapabilityPingPong),
	string(protocol.CapabilityExec),
	string(protocol.CapabilityExecStream),
	string(protocol.CapabilityExecTTY),
	string(protocol.CapabilityIdentity),
	string(protocol.CapabilityReseed),
}

var agentPolicy = policyFromEnvironment()

var auditLog = log.New(os.Stderr, "kumabox-agent: ", log.LstdFlags)

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
	User    string               `json:"user,omitempty"`
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

type reseedRequest struct {
	Type                protocol.RequestType `json:"type"`
	Entropy             []byte               `json:"entropy"`
	RegenerateMachineID bool                 `json:"regenerateMachineId,omitempty"`
}

type reseedResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

var configureIdentity = applyIdentity
var reseedGuest = applyReseed

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
	case protocol.RequestReseed:
		handleReseed(rw, []byte(line))
	default:
		writeResponse(rw, pingResponse{OK: false, Error: "unsupported request"})
	}
}

func handleStreamExec(reader *bufio.Reader, rw io.ReadWriter, request protocol.Frame) {
	if request.TTY {
		handleTTYExec(reader, rw, request)
		return
	}
	if err := validateUser(request.User); err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorUserUnsupported, err.Error())
		return
	}
	if err := validateEnvironment(request.Env, agentPolicy.deniedEnv); err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorEnvDenied, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), agentPolicy.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, request.Args[0], request.Args[1:]...) //nolint:gosec
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	cmd.Dir = request.WorkDir
	cmd.Env = mergeEnvironment(request.Env)
	writer := &streamWriter{writer: rw}
	output := &outputBudget{limit: agentPolicy.maxOutput}
	cmd.Stdout = &streamOutputWriter{writer: writer, id: request.ID, frameType: protocol.FrameStdout, stream: protocol.StreamStdout, budget: output}
	cmd.Stderr = &streamOutputWriter{writer: writer, id: request.ID, frameType: protocol.FrameStderr, stream: protocol.StreamStderr, budget: output}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorExecFailed, err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		writeStreamError(rw, request.ID, protocol.ErrorExecFailed, err.Error())
		return
	}
	processDone := monitorProcess(ctx, cmd)
	defer close(processDone)
	startedAt := time.Now()
	auditLog.Printf("exec start command=%q user=%q", request.Args[0], effectiveUser(request.User))

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
	if ctx.Err() != nil {
		killProcessTree(cmd)
	}
	exitCode := 0
	if waitErr != nil {
		exitCode = commandExitCode(waitErr)
	}
	if ctx.Err() == context.DeadlineExceeded {
		exitCode = 124
		_ = writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameError, ID: request.ID, Code: protocol.ErrorExecTimeout, Message: "execution exceeded policy timeout"})
	}
	if output.exceeded() {
		exitCode = 124
		_ = writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameError, ID: request.ID, Code: protocol.ErrorOutputLimit, Message: "execution output exceeded policy limit"})
	}
	auditLog.Printf("exec end command=%q user=%q exit=%d duration_ms=%d", request.Args[0], effectiveUser(request.User), exitCode, time.Since(startedAt).Milliseconds())
	_ = writer.Write(protocol.Frame{Version: protocol.VersionV1, Type: protocol.FrameExit, ID: request.ID, ExitCode: exitCode})
}

func mergeEnvironment(values map[string]string) []string {
	if len(values) == 0 {
		return os.Environ()
	}
	merged := make(map[string]string, len(os.Environ())+len(values))
	for _, pair := range os.Environ() {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			if _, exists := merged[key]; !exists {
				merged[key] = value
			}
		}
	}
	for key, value := range values {
		merged[key] = value
	}
	result := make([]string, 0, len(merged))
	for key, value := range merged {
		result = append(result, key+"="+value)
	}
	return result
}

func effectiveUser(user string) string {
	if user == "" {
		return "root"
	}
	return user
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
	budget    *outputBudget
}

func (w *streamOutputWriter) Write(data []byte) (int, error) {
	if !w.budget.reserve(int64(len(data))) {
		return 0, fmt.Errorf("%w: maximum output is %d bytes", protocol.ErrorOutputLimit, w.budget.limit)
	}
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

type outputBudget struct {
	mu          sync.Mutex
	limit       int64
	used        int64
	wasExceeded bool
}

func (b *outputBudget) reserve(size int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+size > b.limit {
		b.wasExceeded = true
		return false
	}
	b.used += size
	return true
}

func (b *outputBudget) exceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wasExceeded
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
	auditLog.Printf("identity start hostname=%q interfaces=%d", req.Hostname, len(req.Interfaces))
	if err := configureIdentity(req); err != nil {
		auditLog.Printf("identity failed hostname=%q: %v", req.Hostname, err)
		writeResponse(w, identityResponse{OK: false, Error: err.Error()})
		return
	}
	writeResponse(w, identityResponse{OK: true})
	auditLog.Printf("identity complete hostname=%q", req.Hostname)
}

func handleReseed(w io.Writer, raw []byte) {
	var req reseedRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeResponse(w, reseedResponse{OK: false, Error: "invalid reseed request"})
		return
	}
	if len(req.Entropy) != agentReseedEntropyBytes {
		clear(req.Entropy)
		writeResponse(w, reseedResponse{OK: false, Error: fmt.Sprintf("reseed entropy must be %d bytes", agentReseedEntropyBytes)})
		return
	}
	auditLog.Printf("reseed start regenerate_machine_id=%t", req.RegenerateMachineID)
	err := reseedGuest(req)
	clear(req.Entropy)
	if err != nil {
		auditLog.Printf("reseed failed: %v", err)
		writeResponse(w, reseedResponse{OK: false, Error: err.Error()})
		return
	}
	writeResponse(w, reseedResponse{OK: true})
	auditLog.Printf("reseed complete regenerate_machine_id=%t", req.RegenerateMachineID)
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
	if err := validateUser(req.User); err != nil {
		writeResponse(w, execResponse{OK: false, ExitCode: 126, Error: err.Error()})
		return
	}
	env, err := environmentMapFromPairs(req.Env)
	if err != nil {
		writeResponse(w, execResponse{OK: false, ExitCode: 126, Error: err.Error()})
		return
	}
	if err := validateEnvironment(env, agentPolicy.deniedEnv); err != nil {
		writeResponse(w, execResponse{OK: false, ExitCode: 126, Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), agentPolicy.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, req.Args[0], req.Args[1:]...) //nolint:gosec
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	cmd.Dir = req.WorkDir
	cmd.Env = mergeEnvironment(env)
	cmd.Stdin = bytes.NewReader(req.Stdin)
	stdout := &limitedBuffer{limit: agentPolicy.maxOutput}
	stderr := &limitedBuffer{limit: agentPolicy.maxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	resp := execResponse{OK: true}
	startedAt := time.Now()
	auditLog.Printf("exec start command=%q user=%q", req.Args[0], effectiveUser(req.User))
	if startErr := cmd.Start(); startErr != nil {
		resp.OK = false
		resp.Error = startErr.Error()
		resp.ExitCode = 127
	} else {
		processDone := monitorProcess(ctx, cmd)
		waitErr := cmd.Wait()
		close(processDone)
		if waitErr != nil {
			resp.OK = false
			resp.Error = waitErr.Error()
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				resp.ExitCode = exitErr.ExitCode()
			} else {
				resp.ExitCode = 127
			}
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		resp.OK, resp.ExitCode, resp.Error = false, 124, "EXEC_TIMEOUT: execution exceeded policy timeout"
	}
	if stdout.exceeded || stderr.exceeded {
		resp.OK, resp.ExitCode, resp.Error = false, 124, "OUTPUT_LIMIT: execution output exceeded policy limit"
	}
	resp.Stdout = stdout.Bytes()
	resp.Stderr = stderr.Bytes()
	auditLog.Printf("exec end command=%q user=%q exit=%d duration_ms=%d", req.Args[0], effectiveUser(req.User), resp.ExitCode, time.Since(startedAt).Milliseconds())
	writeResponse(w, resp)
}

func environmentMapFromPairs(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, pair := range values {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("%w: environment must be KEY=VALUE", protocol.ErrorEnvDenied)
		}
		result[key] = value
	}
	return result, nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - int64(b.Len())
	if remaining <= 0 {
		b.exceeded = true
		return 0, fmt.Errorf("%w", protocol.ErrorOutputLimit)
	}
	if int64(len(data)) > remaining {
		data = data[:int(remaining)]
		b.exceeded = true
	}
	return b.Buffer.Write(data)
}

func writeResponse(w io.Writer, resp any) {
	raw, err := json.Marshal(resp)
	if err != nil {
		_, _ = fmt.Fprintln(w, `{"ok":false,"error":"encode response"}`)
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}
