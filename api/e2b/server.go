// Package e2b adapts a focused portion of the E2B control and envd protocols
// to KumaBox. It is isolated from the native v1 API so neither wire contract
// changes the other's resource model.
package e2b

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kumabox/kumabox/api"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

const maxBody = 1 << 20

// Sandboxes is the application capability needed by the E2B adapter.
type Sandboxes interface {
	Run(context.Context, api.CreateSandboxInput) (types.Sandbox, error)
	Inspect(context.Context, string) (types.Sandbox, error)
	Start(context.Context, string) (types.Sandbox, error)
	Stop(context.Context, string) (types.Sandbox, error)
	Remove(context.Context, string) (types.Sandbox, error)
	Exec(context.Context, string, types.Command, io.Reader, io.Writer, io.Writer) (int, error)
}

// Snapshots is the capture and clone capability consumed by E2B snapshot calls.
type Snapshots interface {
	Save(context.Context, api.SaveSnapshotInput) (types.Snapshot, error)
	Hibernate(context.Context, api.SaveSnapshotInput) (types.Snapshot, error)
	List(context.Context) ([]types.Snapshot, error)
	Remove(context.Context, string) (types.Snapshot, error)
	Clone(context.Context, string, string) (types.Sandbox, error)
	Restore(context.Context, string, string) (types.Sandbox, error)
}

type handler struct {
	sandboxes Sandboxes
	snapshots Snapshots
	token     []byte
	nextPID   atomic.Uint32
	leases    *leaseStore
}

// NewHandler exposes E2B create, connect, inspect, kill, health and foreground
// process execution. Other E2B operations are intentionally not advertised.
func NewHandler(sandboxes Sandboxes, snapshots Snapshots, token string) (http.Handler, error) {
	if sandboxes == nil || snapshots == nil || len(token) < 32 {
		return nil, errors.New("E2B adapter requires sandbox and snapshot services and a strong API token")
	}
	h := &handler{sandboxes: sandboxes, snapshots: snapshots, token: []byte(token)}
	leases, err := openLeaseStore()
	if err != nil {
		return nil, fmt.Errorf("open E2B lifecycle state: %w", err)
	}
	h.leases = leases
	if err := leases.load(h.expire); err != nil {
		return nil, fmt.Errorf("load E2B lifecycle state: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/sandboxes", h.create)
	mux.HandleFunc("POST /v2/sandboxes/{id}/connect", h.connect)
	mux.HandleFunc("POST /sandboxes/{id}/pause", h.pause)
	mux.HandleFunc("POST /sandboxes/{id}/resume", h.resume)
	mux.HandleFunc("POST /sandboxes/{id}/timeout", h.setTimeout)
	mux.HandleFunc("GET /sandboxes/{id}", h.inspect)
	mux.HandleFunc("DELETE /sandboxes/{id}", h.kill)
	mux.HandleFunc("POST /sandboxes/{id}/snapshots", h.saveSnapshot)
	mux.HandleFunc("GET /snapshots", h.listSnapshots)
	mux.HandleFunc("DELETE /templates/{id}", h.removeSnapshot)
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /files", h.readFile)
	mux.HandleFunc("POST /files", h.writeFile)
	mux.HandleFunc("POST /process.Process/Start", h.processStart)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/files" || r.URL.Path == "/process.Process/Start" {
			id := r.Header.Get("E2b-Sandbox-Id")
			if id == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Access-Token")), []byte(h.envdToken(id))) != 1 {
				failure(w, http.StatusUnauthorized, "unauthenticated", "invalid sandbox token")
				return
			}
		} else {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			if subtle.ConstantTimeCompare([]byte(key), h.token) != 1 {
				failure(w, http.StatusUnauthorized, "unauthenticated", "invalid API key")
				return
			}
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func (h *handler) envdToken(id string) string {
	mac := hmac.New(sha256.New, h.token)
	_, _ = mac.Write([]byte("envd:" + id))
	return hex.EncodeToString(mac.Sum(nil))
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func failure(w http.ResponseWriter, status int, code, message string) {
	jsonResponse(w, status, map[string]any{"code": code, "message": message})
}

func serviceFailure(w http.ResponseWriter, err error) {
	var classified *errdefs.Error
	if errors.As(err, &classified) {
		status := http.StatusInternalServerError
		switch classified.Class {
		case errdefs.ClassInvalid:
			status = http.StatusBadRequest
		case errdefs.ClassNotFound:
			status = http.StatusNotFound
		case errdefs.ClassConflict:
			status = http.StatusConflict
		case errdefs.ClassUnavailable:
			status = http.StatusServiceUnavailable
		}
		failure(w, status, string(classified.Code), err.Error())
		return
	}
	slog.Error("E2B adapter failure", "error", err)
	failure(w, http.StatusInternalServerError, "internal", "internal error")
}

type createRequest struct {
	TemplateID string            `json:"templateID"`
	Timeout    *int              `json:"timeout"`
	Metadata   map[string]string `json:"metadata"`
	EnvVars    map[string]string `json:"envVars"`
	AutoPause  *bool             `json:"autoPause"`
	Network    json.RawMessage   `json:"network"`
	MCP        json.RawMessage   `json:"mcp"`
	IAM        json.RawMessage   `json:"iam"`
	Volumes    json.RawMessage   `json:"volumeMounts"`
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON request")
	}
	return nil
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	var input createRequest
	if err := decodeJSON(w, r, &input); err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	if input.TemplateID == "" {
		failure(w, http.StatusBadRequest, "invalid_argument", "templateID is required; use an imported KumaBox image alias")
		return
	}
	if len(input.Metadata) != 0 || len(input.EnvVars) != 0 || len(input.Network) != 0 || len(input.MCP) != 0 || len(input.IAM) != 0 || len(input.Volumes) != 0 {
		failure(w, http.StatusNotImplemented, "unimplemented", "E2B metadata, envs, network policy, MCP, IAM and volume mounts are not supported by this adapter")
		return
	}
	timeout := defaultTimeout
	if input.Timeout != nil {
		if *input.Timeout <= 0 || *input.Timeout > 86400 {
			failure(w, http.StatusBadRequest, "invalid_argument", "timeout must be between 1 and 86400 seconds")
			return
		}
		timeout = time.Duration(*input.Timeout) * time.Second
	}
	id, err := types.NewSandboxID()
	if err != nil {
		serviceFailure(w, err)
		return
	}
	name := "e2b-" + id.String()
	var record types.Sandbox
	if _, parseErr := types.ParseSnapshotID(input.TemplateID); parseErr == nil {
		record, err = h.snapshots.Clone(r.Context(), input.TemplateID, name)
	} else {
		record, err = h.sandboxes.Run(r.Context(), api.CreateSandboxInput{
			ImageReference: input.TemplateID,
			Config: types.SandboxConfig{
				Name: name, CPUs: types.DefaultSandboxCPUs, Memory: types.DefaultSandboxMemory,
				Storage: types.DefaultSandboxStorage, NICs: 1,
			},
		})
	}
	if err != nil {
		if record.ID != "" {
			err = fmt.Errorf("retained sandbox %s: %w", record.ID, err)
		}
		serviceFailure(w, err)
		return
	}
	policy := lease{ExpiresAt: time.Now().UTC().Add(timeout), AutoPause: input.AutoPause != nil && *input.AutoPause}
	if err := h.leases.put(record.ID, policy); err != nil {
		// A sandbox without its expiry policy must not escape as an E2B resource.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		_, stopErr := h.sandboxes.Stop(cleanupCtx, record.ID.String())
		_, removeErr := h.sandboxes.Remove(cleanupCtx, record.ID.String())
		serviceFailure(w, errors.Join(fmt.Errorf("persist E2B lease: %w", err), stopErr, removeErr))
		return
	}
	h.leases.schedule(record.ID, policy.ExpiresAt, h.expire)
	jsonResponse(w, http.StatusCreated, h.sandbox(record, input.TemplateID))
}

func (h *handler) saveSnapshot(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name   string `json:"name"`
		Memory *bool  `json:"memory"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	if input.Memory != nil && !*input.Memory {
		failure(w, http.StatusNotImplemented, "unimplemented", "filesystem-only E2B snapshots are not supported")
		return
	}
	record, err := h.snapshots.Save(r.Context(), api.SaveSnapshotInput{
		SandboxReference: r.PathValue("id"), Name: input.Name,
	})
	if err != nil {
		serviceFailure(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, snapshotInfo(record))
}

func (h *handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	records, err := h.snapshots.List(r.Context())
	if err != nil {
		serviceFailure(w, err)
		return
	}
	result := make([]map[string]any, 0, len(records))
	for _, record := range records {
		if record.Description == pauseDescription {
			continue
		}
		if id := r.URL.Query().Get("sandboxID"); id != "" && id != record.SandboxID.String() {
			continue
		}
		if name := r.URL.Query().Get("name"); name != "" && name != record.Name && name != record.ID.String() {
			continue
		}
		result = append(result, snapshotInfo(record))
	}
	jsonResponse(w, http.StatusOK, result)
}

func (h *handler) removeSnapshot(w http.ResponseWriter, r *http.Request) {
	if _, err := h.snapshots.Remove(r.Context(), r.PathValue("id")); err != nil {
		serviceFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func snapshotInfo(record types.Snapshot) map[string]any {
	names := []string{}
	if record.Name != "" {
		names = append(names, record.Name)
	}
	return map[string]any{"snapshotID": record.ID.String(), "names": names}
}

func (h *handler) sandbox(record types.Sandbox, template string) map[string]any {
	return map[string]any{
		"sandboxID": record.ID.String(), "templateID": template,
		"clientID": "", "envdVersion": "0.1.0", "envdAccessToken": h.envdToken(record.ID.String()),
	}
}

func (h *handler) connect(w http.ResponseWriter, r *http.Request) {
	h.connectWithStatus(w, r, http.StatusOK)
}

func (h *handler) connectWithStatus(w http.ResponseWriter, r *http.Request, status int) {
	id, ok := sandboxID(w, r)
	if !ok {
		return
	}
	timeout := defaultTimeout
	if r.ContentLength != 0 {
		var input struct {
			Timeout *int  `json:"timeout"`
			Memory  *bool `json:"memory"`
		}
		if err := decodeJSON(w, r, &input); err != nil {
			failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		if input.Memory != nil && !*input.Memory {
			failure(w, http.StatusNotImplemented, "unimplemented", "filesystem-only resume is not supported")
			return
		}
		if input.Timeout != nil {
			if *input.Timeout <= 0 || *input.Timeout > 86400 {
				failure(w, http.StatusBadRequest, "invalid_argument", "timeout must be between 1 and 86400 seconds")
				return
			}
			timeout = time.Duration(*input.Timeout) * time.Second
		}
	}
	record, err := h.connectSandbox(r.Context(), id, timeout)
	if err != nil {
		serviceFailure(w, err)
		return
	}
	jsonResponse(w, status, h.sandbox(record, record.ImageDigest.String()))
}

func (h *handler) inspect(w http.ResponseWriter, r *http.Request) {
	record, err := h.sandboxes.Inspect(r.Context(), r.PathValue("id"))
	if err != nil {
		serviceFailure(w, err)
		return
	}
	policy, exists, err := h.leases.get(record.ID)
	if err != nil {
		serviceFailure(w, err)
		return
	}
	if !exists {
		failure(w, http.StatusNotFound, "not_found", "E2B sandbox lease not found")
		return
	}
	state := "running"
	if record.State == types.SandboxStateStopped && policy.PausedSnapshot != "" {
		state = "paused"
	} else if record.State != types.SandboxStateRunning {
		failure(w, http.StatusConflict, "state_conflict", "sandbox is not running or paused")
		return
	}
	endAt := policy.ExpiresAt
	if endAt.IsZero() {
		endAt = record.UpdatedAt
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"sandboxID": record.ID.String(), "templateID": record.ImageDigest.String(),
		"clientID": "", "envdVersion": "0.1.0", "envdAccessToken": h.envdToken(record.ID.String()),
		"startedAt": record.CreatedAt, "endAt": endAt,
		"state": state, "cpuCount": record.Config.CPUs,
		"memoryMB": record.Config.Memory / (1 << 20), "diskSizeMB": record.Config.Storage / (1 << 20),
		"metadata": map[string]string{},
	})
}

func (h *handler) kill(w http.ResponseWriter, r *http.Request) {
	id, ok := sandboxID(w, r)
	if !ok {
		return
	}
	unlock, err := h.leases.lock(r.Context(), id)
	if err != nil {
		serviceFailure(w, err)
		return
	}
	defer unlock()
	policy, exists, err := h.leases.get(id)
	if err != nil {
		serviceFailure(w, err)
		return
	}
	if !exists {
		failure(w, http.StatusNotFound, "not_found", "E2B sandbox lease not found")
		return
	}
	if err := h.killSandbox(r.Context(), id, policy); err != nil {
		serviceFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	record, err := h.sandboxes.Inspect(r.Context(), r.Header.Get("E2b-Sandbox-Id"))
	if err != nil || record.State != types.SandboxStateRunning {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// connectWriter emits Connect's five-byte length-prefixed JSON envelopes.
type connectWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

func (w *connectWriter) message(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return w.frame(0, payload)
}

func (w *connectWriter) frame(flags byte, payload []byte) error {
	if uint64(len(payload)) > uint64(^uint32(0)) {
		return errors.New("connect frame exceeds uint32 length")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var header [5]byte
	header[0] = flags
	// #nosec G115 -- the payload length is checked above.
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.w.Write(payload); err != nil {
		return err
	}
	w.flusher.Flush()
	return nil
}

func (w *connectWriter) end(code, message string) {
	payload := []byte("{}")
	if code != "" {
		payload, _ = json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	}
	_ = w.frame(2, payload)
}

type outputWriter struct {
	stream string
	w      *connectWriter
}

func (o outputWriter) Write(data []byte) (int, error) {
	if err := o.w.message(map[string]any{"event": map[string]any{"data": map[string]string{o.stream: base64.StdEncoding.EncodeToString(data)}}}); err != nil {
		return 0, err
	}
	return len(data), nil
}

type processStartRequest struct {
	Process struct {
		Cmd  string            `json:"cmd"`
		Args []string          `json:"args"`
		Envs map[string]string `json:"envs"`
		CWD  string            `json:"cwd"`
	} `json:"process"`
	PTY   json.RawMessage `json:"pty"`
	Stdin bool            `json:"stdin"`
}

// processStart translates one foreground Connect stream into the guest command
// channel. No guest-side envd process is required for this supported subset.
//
//	E2B SDK -- framed StartRequest --> adapter -- Exec --> guest agent
//	E2B SDK <-- Start/Data/End frames -- adapter <-- stdout/stderr/exit
func (h *handler) processStart(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/connect+json" {
		failure(w, http.StatusUnsupportedMediaType, "invalid_argument", "Connect JSON streaming is required")
		return
	}
	var header [5]byte
	if _, err := io.ReadFull(r.Body, header[:]); err != nil || header[0] != 0 {
		failure(w, http.StatusBadRequest, "invalid_argument", "invalid Connect request frame")
		return
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length == 0 || length > maxBody {
		failure(w, http.StatusBadRequest, "invalid_argument", "invalid Connect request size")
		return
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r.Body, payload); err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", "truncated Connect request")
		return
	}
	var input processStartRequest
	if err := json.Unmarshal(payload, &input); err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	if input.Process.Cmd == "" || input.Process.CWD != "" || len(input.PTY) != 0 || input.Stdin {
		failure(w, http.StatusNotImplemented, "unimplemented", "only foreground commands without PTY, stdin or custom cwd are supported")
		return
	}
	command := types.Command{Args: append([]string{input.Process.Cmd}, input.Process.Args...), Env: input.Process.Envs}
	if err := command.Validate(); err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		failure(w, http.StatusInternalServerError, "internal", "streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/connect+json")
	w.Header().Set("Connect-Protocol-Version", "1")
	w.WriteHeader(http.StatusOK)
	stream := &connectWriter{w: w, flusher: flusher}
	pid := h.nextPID.Add(1)
	if err := stream.message(map[string]any{"event": map[string]any{"start": map[string]any{"pid": pid}}}); err != nil {
		return
	}
	exitCode, err := h.sandboxes.Exec(r.Context(), r.Header.Get("E2b-Sandbox-Id"), command, strings.NewReader(""),
		outputWriter{stream: "stdout", w: stream}, outputWriter{stream: "stderr", w: stream})
	if err != nil {
		stream.end("unavailable", fmt.Sprintf("guest command failed: %v", err))
		return
	}
	if err := stream.message(map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": exitCode, "exited": true}}}); err != nil {
		return
	}
	stream.end("", "")
}
