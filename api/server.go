package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

const maxRequestBytes = 1 << 20

// SandboxService is the local capability required by the HTTP sandbox routes.
type SandboxService interface {
	Create(context.Context, CreateSandboxInput) (types.Sandbox, error)
	Run(context.Context, CreateSandboxInput) (types.Sandbox, error)
	List(context.Context, bool) ([]types.Sandbox, error)
	Inspect(context.Context, string) (types.Sandbox, error)
	Start(context.Context, string) (types.Sandbox, error)
	Stop(context.Context, string) (types.Sandbox, error)
	Remove(context.Context, string) (types.Sandbox, error)
	Exec(context.Context, string, types.Command, io.Reader, io.Writer, io.Writer) (int, error)
}

// SnapshotService is the local capability required by snapshot routes.
type SnapshotService interface {
	Save(context.Context, SaveSnapshotInput) (types.Snapshot, error)
	Hibernate(context.Context, SaveSnapshotInput) (types.Snapshot, error)
	List(context.Context) ([]types.Snapshot, error)
	Inspect(context.Context, string) (types.Snapshot, error)
	Remove(context.Context, string) (types.Snapshot, error)
	Clone(context.Context, string, string) (types.Sandbox, error)
	Restore(context.Context, string, string) (types.Sandbox, error)
}

// Services groups the independent application capabilities used by the API.
type Services struct {
	Sandboxes SandboxService
	Snapshots SnapshotService
}

// NewHandler constructs the authenticated v1 HTTP API. The token is mandatory
// because all mutations run with the server's host privileges.
func NewHandler(services Services, token string) (http.Handler, error) {
	if services.Sandboxes == nil || services.Snapshots == nil {
		return nil, errors.New("API services are incomplete")
	}
	if len(token) < 32 || strings.TrimSpace(token) != token {
		return nil, errors.New("API token must contain at least 32 non-whitespace characters")
	}
	h := &handler{services: services, token: []byte(token)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", h.listSandboxes)
	mux.HandleFunc("POST /v1/sandboxes", h.createSandbox)
	mux.HandleFunc("GET /v1/sandboxes/{ref}", h.inspectSandbox)
	mux.HandleFunc("DELETE /v1/sandboxes/{ref}", h.removeSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{ref}/start", h.startSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{ref}/stop", h.stopSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{ref}/exec", h.execSandbox)
	mux.HandleFunc("GET /v1/sandboxes/{ref}/files", h.readSandboxFile)
	mux.HandleFunc("POST /v1/sandboxes/{ref}/files", h.writeSandboxFile)
	mux.HandleFunc("POST /v1/sandboxes/{ref}/restore", h.restoreSandbox)
	mux.HandleFunc("GET /v1/snapshots", h.listSnapshots)
	mux.HandleFunc("POST /v1/snapshots", h.saveSnapshot)
	mux.HandleFunc("GET /v1/snapshots/{ref}", h.inspectSnapshot)
	mux.HandleFunc("DELETE /v1/snapshots/{ref}", h.removeSnapshot)
	mux.HandleFunc("POST /v1/snapshots/{ref}/clone", h.cloneSnapshot)
	mux.HandleFunc("POST /v1/sandboxes/{ref}/hibernate", h.hibernateSandbox)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if provided == r.Header.Get("Authorization") {
			provided = r.Header.Get("X-API-Key")
		}
		if subtle.ConstantTimeCompare([]byte(provided), h.token) != 1 {
			writeFailure(w, http.StatusUnauthorized, Error{Code: "UNAUTHORIZED", Message: "invalid API token"})
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

type handler struct {
	services Services
	token    []byte
}

func decode(w http.ResponseWriter, r *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request body must contain exactly one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("write API response", "error", err)
	}
}

func writeFailure(w http.ResponseWriter, status int, failure Error) {
	writeJSON(w, status, ErrorResponse{Error: failure})
}

func invalidRequest(w http.ResponseWriter, err error) {
	writeFailure(w, http.StatusBadRequest, Error{Code: string(errdefs.CodeInvalidArgument), Message: err.Error()})
}

func domainFailure(w http.ResponseWriter, err error) {
	domainFailureWithResource(w, err, "")
}

func domainFailureWithResource(w http.ResponseWriter, err error, resourceID string) {
	var classified *errdefs.Error
	if !errors.As(err, &classified) {
		slog.Error("unclassified API failure", "error", err)
		writeFailure(w, http.StatusInternalServerError, Error{Code: string(errdefs.CodeInternal), Message: "internal error"})
		return
	}
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
	writeFailure(w, status, Error{Code: string(classified.Code), Message: err.Error(), Committed: classified.Committed, ResourceID: resourceID})
}

func (h *handler) listSandboxes(w http.ResponseWriter, r *http.Request) {
	records, err := h.services.Sandboxes.List(r.Context(), true)
	if err != nil {
		domainFailure(w, err)
		return
	}
	result := make([]Sandbox, 0, len(records))
	for _, record := range records {
		result = append(result, sandboxRecord(record))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) createSandbox(w http.ResponseWriter, r *http.Request) {
	var request CreateSandboxRequest
	if err := decode(w, r, &request); err != nil {
		invalidRequest(w, err)
		return
	}
	config := types.SandboxConfig{
		Name: request.Name, CPUs: types.DefaultSandboxCPUs, Memory: types.DefaultSandboxMemory,
		Storage: types.DefaultSandboxStorage, NICs: 1, NetworkName: request.Network,
		SharedMemory: request.SharedMemory,
	}
	if config.Name == "" {
		id, err := types.NewSandboxID()
		if err != nil {
			domainFailure(w, err)
			return
		}
		config.Name = "kb-" + id.String()
	}
	if request.CPUs != nil {
		config.CPUs = *request.CPUs
	}
	if request.Memory != nil {
		config.Memory = *request.Memory
	}
	if request.Storage != nil {
		config.Storage = *request.Storage
	}
	if request.NICs != nil {
		config.NICs = *request.NICs
	}
	for _, disk := range request.DataDisks {
		config.DataDisks = append(config.DataDisks, types.DataDiskSpec{
			Name: disk.Name, Size: disk.Size, FSType: disk.FSType, DirectIO: disk.DirectIO,
		})
	}
	if request.Image == "" {
		invalidRequest(w, errors.New("image is required"))
		return
	}
	if err := config.Validate(); err != nil {
		invalidRequest(w, err)
		return
	}
	input := CreateSandboxInput{ImageReference: request.Image, Config: config}
	start := request.Start == nil || *request.Start
	var record types.Sandbox
	var err error
	if start {
		record, err = h.services.Sandboxes.Run(r.Context(), input)
	} else {
		record, err = h.services.Sandboxes.Create(r.Context(), input)
	}
	if err != nil {
		domainFailureWithResource(w, err, record.ID.String())
		return
	}
	writeJSON(w, http.StatusCreated, sandboxRecord(record))
}

func (h *handler) inspectSandbox(w http.ResponseWriter, r *http.Request) {
	record, err := h.services.Sandboxes.Inspect(r.Context(), r.PathValue("ref"))
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxRecord(record))
}

func (h *handler) removeSandbox(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	current, err := h.services.Sandboxes.Inspect(r.Context(), ref)
	if err != nil {
		domainFailure(w, err)
		return
	}
	if current.State == types.SandboxStateRunning {
		if _, err := h.services.Sandboxes.Stop(r.Context(), ref); err != nil {
			domainFailure(w, err)
			return
		}
	}
	record, err := h.services.Sandboxes.Remove(r.Context(), ref)
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxRecord(record))
}

func (h *handler) startSandbox(w http.ResponseWriter, r *http.Request) {
	record, err := h.services.Sandboxes.Start(r.Context(), r.PathValue("ref"))
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxRecord(record))
}

func (h *handler) stopSandbox(w http.ResponseWriter, r *http.Request) {
	record, err := h.services.Sandboxes.Stop(r.Context(), r.PathValue("ref"))
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxRecord(record))
}

// eventWriter serializes stdout and stderr from the guest agent into NDJSON.
type eventWriter struct {
	mu      *sync.Mutex
	encoder *json.Encoder
	flusher http.Flusher
	stream  string
}

func (w *eventWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.encoder.Encode(ExecEvent{Stream: w.stream, Data: base64.StdEncoding.EncodeToString(data)}); err != nil {
		return 0, err
	}
	w.flusher.Flush()
	return len(data), nil
}

func (h *handler) execSandbox(w http.ResponseWriter, r *http.Request) {
	var request ExecRequest
	if err := decode(w, r, &request); err != nil {
		invalidRequest(w, err)
		return
	}
	command := types.Command{Args: request.Args, Env: request.Env}
	if err := command.Validate(); err != nil {
		invalidRequest(w, err)
		return
	}
	input, err := base64.StdEncoding.DecodeString(request.Stdin)
	if err != nil {
		invalidRequest(w, fmt.Errorf("stdin must be base64: %w", err))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeFailure(w, http.StatusInternalServerError, Error{Code: string(errdefs.CodeInternal), Message: "streaming unavailable"})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	// A shared mutex prevents stdout and stderr events from interleaving.
	shared := &sync.Mutex{}
	stdout := &eventWriter{mu: shared, encoder: encoder, flusher: flusher, stream: "stdout"}
	stderr := &eventWriter{mu: shared, encoder: encoder, flusher: flusher, stream: "stderr"}
	exitCode, runErr := h.services.Sandboxes.Exec(r.Context(), r.PathValue("ref"), command, bytes.NewReader(input), stdout, stderr)
	shared.Lock()
	defer shared.Unlock()
	if runErr != nil {
		failure := errorForStream(runErr)
		_ = encoder.Encode(ExecEvent{Error: &failure})
	} else {
		_ = encoder.Encode(ExecEvent{ExitCode: &exitCode})
	}
	flusher.Flush()
}

func errorForStream(err error) Error {
	var classified *errdefs.Error
	if errors.As(err, &classified) {
		return Error{Code: string(classified.Code), Message: err.Error(), Committed: classified.Committed}
	}
	slog.Error("unclassified stream failure", "error", err)
	return Error{Code: string(errdefs.CodeInternal), Message: "internal error"}
}

func (h *handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	records, err := h.services.Snapshots.List(r.Context())
	if err != nil {
		domainFailure(w, err)
		return
	}
	result := make([]Snapshot, 0, len(records))
	for _, record := range records {
		result = append(result, snapshotRecord(record))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *handler) saveSnapshot(w http.ResponseWriter, r *http.Request) {
	var request SaveSnapshotRequest
	if err := decode(w, r, &request); err != nil {
		invalidRequest(w, err)
		return
	}
	record, err := h.services.Snapshots.Save(r.Context(), SaveSnapshotInput{
		SandboxReference: request.Sandbox, Name: request.Name, Description: request.Description,
	})
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, snapshotRecord(record))
}

func (h *handler) hibernateSandbox(w http.ResponseWriter, r *http.Request) {
	var request SaveSnapshotRequest
	if err := decode(w, r, &request); err != nil {
		invalidRequest(w, err)
		return
	}
	record, err := h.services.Snapshots.Hibernate(r.Context(), SaveSnapshotInput{
		SandboxReference: r.PathValue("ref"), Name: request.Name, Description: request.Description,
	})
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, snapshotRecord(record))
}

func (h *handler) inspectSnapshot(w http.ResponseWriter, r *http.Request) {
	record, err := h.services.Snapshots.Inspect(r.Context(), r.PathValue("ref"))
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshotRecord(record))
}

func (h *handler) removeSnapshot(w http.ResponseWriter, r *http.Request) {
	record, err := h.services.Snapshots.Remove(r.Context(), r.PathValue("ref"))
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshotRecord(record))
}

func (h *handler) cloneSnapshot(w http.ResponseWriter, r *http.Request) {
	var request CloneRequest
	if err := decode(w, r, &request); err != nil {
		invalidRequest(w, err)
		return
	}
	record, err := h.services.Snapshots.Clone(r.Context(), r.PathValue("ref"), request.Name)
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sandboxRecord(record))
}

func (h *handler) restoreSandbox(w http.ResponseWriter, r *http.Request) {
	var request RestoreRequest
	if err := decode(w, r, &request); err != nil {
		invalidRequest(w, err)
		return
	}
	record, err := h.services.Snapshots.Restore(r.Context(), r.PathValue("ref"), request.Snapshot)
	if err != nil {
		domainFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxRecord(record))
}
