package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"

	"github.com/kumabox/kumabox/types"
)

func (h *handler) fileTarget(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	ref := r.PathValue("ref")
	record, err := h.services.Sandboxes.Inspect(r.Context(), ref)
	if err != nil {
		domainFailure(w, err)
		return "", "", false
	}
	if record.State != types.SandboxStateRunning {
		writeFailure(w, http.StatusConflict, Error{Code: "STATE_CONFLICT", Message: "sandbox is not running"})
		return "", "", false
	}
	filePath, err := GuestFilePath(r.URL.Query().Get("path"))
	if err != nil {
		invalidRequest(w, err)
		return "", "", false
	}
	return ref, filePath, true
}

func (h *handler) readSandboxFile(w http.ResponseWriter, r *http.Request) {
	ref, filePath, ok := h.fileTarget(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	output := &fileResponseWriter{writer: w}
	if err := ReadGuestFile(r.Context(), h.services.Sandboxes, ref, filePath, output); err != nil {
		if output.written != 0 {
			slog.Error("native API file read failed after streaming", "sandbox", ref, "error", err)
			return
		}
		if errors.Is(err, ErrGuestFileNotFound) {
			writeFailure(w, http.StatusNotFound, Error{Code: "ARTIFACT_UNAVAILABLE", Message: err.Error()})
		} else {
			domainFailure(w, err)
		}
	}
}

type fileResponseWriter struct {
	writer  io.Writer
	written int64
}

func (w *fileResponseWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.written += int64(n)
	return n, err
}

func (h *handler) writeSandboxFile(w http.ResponseWriter, r *http.Request) {
	ref, filePath, ok := h.fileTarget(w, r)
	if !ok {
		return
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Content-Encoding") != "" {
		writeFailure(w, http.StatusUnsupportedMediaType, Error{Code: "INVALID_ARGUMENT", Message: "file upload requires uncompressed application/octet-stream"})
		return
	}
	upload, err := SpoolGuestUpload(r.Body)
	if err != nil {
		invalidRequest(w, err)
		return
	}
	defer func() {
		_ = upload.Close()
		_ = os.Remove(upload.Name())
	}()
	if err := WriteGuestFile(r.Context(), h.services.Sandboxes, ref, filePath, upload); err != nil {
		if errors.Is(err, ErrGuestFileWrite) {
			writeFailure(w, http.StatusConflict, Error{Code: "GUEST_FILE_ERROR", Message: err.Error()})
		} else {
			domainFailure(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": path.Base(filePath), "path": filePath})
}
