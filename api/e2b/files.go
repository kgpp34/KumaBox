package e2b

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"

	"github.com/kumabox/kumabox/api"
	"github.com/kumabox/kumabox/types"
)

func (h *handler) fileSandbox(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.Header.Get("E2b-Sandbox-Id")
	record, err := h.sandboxes.Inspect(r.Context(), id)
	if err != nil {
		serviceFailure(w, err)
		return "", false
	}
	if record.State != types.SandboxStateRunning {
		failure(w, http.StatusConflict, "state_conflict", "sandbox is not running")
		return "", false
	}
	return id, true
}

func (h *handler) readFile(w http.ResponseWriter, r *http.Request) {
	id, ok := h.fileSandbox(w, r)
	if !ok {
		return
	}
	filePath, err := api.GuestFilePath(r.URL.Query().Get("path"))
	if err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	output := &countedWriter{writer: w}
	if err := api.ReadGuestFile(r.Context(), h.sandboxes, id, filePath, output); err != nil {
		if output.count == 0 {
			if errors.Is(err, api.ErrGuestFileNotFound) {
				failure(w, http.StatusNotFound, "not_found", err.Error())
			} else {
				serviceFailure(w, err)
			}
		} else {
			// The body is already streaming; a second JSON status is impossible.
			slog.Error("E2B file read failed after response started", "sandbox", id, "error", err)
		}
	}
}

type countedWriter struct {
	writer io.Writer
	count  int64
}

func (w *countedWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.count += int64(n)
	return n, err
}

// spoolUpload validates a complete bounded upload before mutating the guest.
// This prevents an oversized request from leaving a truncated guest file.
func spoolUpload(r *http.Request) (string, *os.File, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return "", nil, err
	}
	if r.Header.Get("Content-Encoding") != "" {
		return "", nil, fmt.Errorf("compressed uploads are not supported")
	}
	var source io.Reader = r.Body
	var multipartReader *multipart.Reader
	filePath := r.URL.Query().Get("path")
	if mediaType == "multipart/form-data" {
		reader, err := r.MultipartReader()
		if err != nil {
			return "", nil, err
		}
		part, err := reader.NextPart()
		if err != nil {
			return "", nil, err
		}
		defer func() { _ = part.Close() }()
		if part.FormName() != "file" {
			return "", nil, fmt.Errorf("multipart upload requires a file part")
		}
		if filePath == "" {
			filePath = part.FileName()
		}
		source = part
		multipartReader = reader
	} else if mediaType != "application/octet-stream" {
		return "", nil, fmt.Errorf("unsupported file upload content type %q", mediaType)
	}
	filePath, err = api.GuestFilePath(filePath)
	if err != nil {
		return "", nil, err
	}
	file, err := api.SpoolGuestUpload(source)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = file.Close(); _ = os.Remove(file.Name()) }
	if multipartReader != nil {
		if extra, err := multipartReader.NextPart(); err != io.EOF {
			if extra != nil {
				_ = extra.Close()
			}
			cleanup()
			if err != nil {
				return "", nil, err
			}
			return "", nil, fmt.Errorf("multipart upload supports one file per request")
		}
	}
	return filePath, file, nil
}

func (h *handler) writeFile(w http.ResponseWriter, r *http.Request) {
	id, ok := h.fileSandbox(w, r)
	if !ok {
		return
	}
	filePath, upload, err := spoolUpload(r)
	if err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	defer func() {
		_ = upload.Close()
		_ = os.Remove(upload.Name())
	}()
	if err := api.WriteGuestFile(r.Context(), h.sandboxes, id, filePath, upload); err != nil {
		if errors.Is(err, api.ErrGuestFileWrite) {
			failure(w, http.StatusConflict, "guest_file_error", err.Error())
		} else {
			serviceFailure(w, err)
		}
		return
	}
	jsonResponse(w, http.StatusOK, []map[string]string{{"name": path.Base(filePath), "path": filePath, "type": "file"}})
}
