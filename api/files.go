package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/kumabox/kumabox/types"
)

// GuestExecutor is the existing command capability needed for file transfer.
// The path is always an argv element, never interpolated into shell source.
type GuestExecutor interface {
	Exec(context.Context, string, types.Command, io.Reader, io.Writer, io.Writer) (int, error)
}

var (
	// ErrGuestFileNotFound means the requested regular file is absent.
	ErrGuestFileNotFound = errors.New("guest file is not a regular file")
	// ErrGuestFileWrite means the guest rejected a file upload.
	ErrGuestFileWrite = errors.New("guest file write failed")
)

// MaxGuestUploadBytes bounds temporary host storage and guest file writes.
const MaxGuestUploadBytes = 64 << 20

// SpoolGuestUpload validates the whole payload before any guest mutation.
// The caller must close and remove the returned temporary file.
func SpoolGuestUpload(source io.Reader) (*os.File, error) {
	file, err := os.CreateTemp("", "kumabox-guest-upload-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	length, err := io.Copy(file, io.LimitReader(source, MaxGuestUploadBytes+1))
	if err != nil || length > MaxGuestUploadBytes {
		cleanup()
		if err == nil {
			err = fmt.Errorf("upload exceeds %d bytes", MaxGuestUploadBytes)
		}
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	return file, nil
}

// GuestFilePath accepts only unambiguous absolute guest paths.
func GuestFilePath(value string) (string, error) {
	if value == "" || !path.IsAbs(value) || strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("file path must be an absolute guest path without NUL bytes")
	}
	clean := path.Clean(value)
	if clean == "/" {
		return "", fmt.Errorf("file path must identify a file, not the guest root")
	}
	return clean, nil
}

// ReadGuestFile checks existence before streaming bytes over the existing
// guest command channel. Readers should prepare their HTTP status before
// passing the response writer, because later I/O failures cannot change it.
func ReadGuestFile(ctx context.Context, executor GuestExecutor, ref, filePath string, out io.Writer) error {
	probe := types.Command{Args: []string{"/bin/sh", "-c", "test -f \"$1\"", "--", filePath}}
	code, err := executor.Exec(ctx, ref, probe, nil, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if code != 0 {
		return ErrGuestFileNotFound
	}
	read := types.Command{Args: []string{"/bin/sh", "-c", "exec cat -- \"$1\"", "--", filePath}}
	code, err = executor.Exec(ctx, ref, read, nil, out, io.Discard)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("guest file read exited with status %d", code)
	}
	return nil
}

// WriteGuestFile streams bounded, prevalidated upload bytes to the guest.
// The caller owns size limits and closes input after this operation.
func WriteGuestFile(ctx context.Context, executor GuestExecutor, ref, filePath string, input io.Reader) error {
	write := types.Command{Args: []string{"/bin/sh", "-c", "mkdir -p -- \"$1\" && cat > \"$2\"", "--", path.Dir(filePath), filePath}}
	code, err := executor.Exec(ctx, ref, write, input, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%w: exit status %d", ErrGuestFileWrite, code)
	}
	return nil
}
