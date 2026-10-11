package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/kumabox/kumabox/types"
)

var errInvalidFileRequest = errors.New("invalid file request")

func validateFileRequest(request Message) error {
	destructive := request.Type == MessageFileWrite || request.Type == MessageFileRemove || request.Type == MessageFileRename
	if err := types.ValidateGuestPath(request.Path, destructive); err != nil {
		return fmt.Errorf("%w: %w", errInvalidFileRequest, err)
	}
	if request.Type == MessageFileRename {
		if err := types.ValidateGuestPath(request.Destination, true); err != nil {
			return fmt.Errorf("%w: %w", errInvalidFileRequest, err)
		}
	}
	if err := (types.FileWriteOptions{Mode: request.Mode}).Validate(); err != nil {
		return fmt.Errorf("%w: %w", errInvalidFileRequest, err)
	}
	return nil
}

func fileFailure(encoder *Encoder, err error) {
	code := "IO_ERROR"
	switch {
	case errors.Is(err, errInvalidFileRequest):
		code = "INVALID_ARGUMENT"
	case errors.Is(err, os.ErrNotExist):
		code = "NOT_FOUND"
	case errors.Is(err, os.ErrExist):
		code = "CONFLICT"
	case errors.Is(err, os.ErrPermission):
		code = "PERMISSION_DENIED"
	}
	_ = encoder.Encode(Message{Type: MessageError, Code: code, Message: err.Error()})
}

// serveFile acts within the guest owner's filesystem authority, just like exec.
// It never interprets paths as shell text or accesses a host-side path itself.
func serveFile(ctx context.Context, decoder *Decoder, encoder *Encoder, request Message) error {
	if err := validateFileRequest(request); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var err error
	switch request.Type {
	case MessageFileRead:
		err = readGuestFile(ctx, encoder, request.Path)
	case MessageFileWrite:
		err = writeGuestFile(ctx, decoder, encoder, request)
	case MessageFileList:
		err = listGuestFiles(ctx, encoder, request.Path)
	case MessageFileStat:
		var info types.FileInfo
		info, err = guestFileInfo(request.Path)
		if err == nil {
			err = encoder.Encode(Message{Type: MessageFileInfo, File: &info})
		}
	case MessageFileMkdir:
		if request.Parents {
			err = os.MkdirAll(request.Path, 0o700)
		} else {
			err = os.Mkdir(request.Path, 0o700)
		}
	case MessageFileRemove:
		if request.Recursive {
			err = os.RemoveAll(request.Path)
		} else {
			err = os.Remove(request.Path)
		}
	case MessageFileRename:
		err = os.Rename(request.Path, request.Destination)
	}
	if err != nil {
		return err
	}
	return encoder.Encode(Message{Type: MessageDone})
}

func guestFileInfo(name string) (types.FileInfo, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return types.FileInfo{}, err
	}
	result := types.FileInfo{Name: info.Name(), Path: name, Size: info.Size(), Mode: uint32(info.Mode().Perm()), ModifiedAt: info.ModTime().UTC(), Kind: "other"}
	switch {
	case info.Mode().IsRegular():
		result.Kind = "file"
	case info.IsDir():
		result.Kind = "directory"
	case info.Mode()&os.ModeSymlink != 0:
		result.Kind = "symlink"
		result.LinkTarget, err = os.Readlink(name)
	}
	return result, err
}

// openGuestEntry uses nonblocking open before fstat, so FIFO inputs cannot
// hang the agent. Reads follow symlinks but accept only regular files.
func openGuestEntry(name string, directory bool) (*os.File, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // owner-authorized paths are inside the guest, not the host
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && ((directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular())) {
		err = fmt.Errorf("%w: expected a regular file or directory matching the operation", errInvalidFileRequest)
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func readGuestFile(ctx context.Context, encoder *Encoder, name string) error {
	file, err := openGuestEntry(name, false)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // read-only descriptor
	buffer := make([]byte, streamChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := file.Read(buffer)
		if n > 0 {
			if sendErr := encoder.Encode(Message{Type: MessageFileData, Data: buffer[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func listGuestFiles(ctx context.Context, encoder *Encoder, name string) error {
	directory, err := openGuestEntry(name, true)
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck // read-only descriptor
	for {
		entries, err := directory.ReadDir(fileListBatchSize)
		batch := make([]types.FileInfo, 0, len(entries))
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			info, statErr := guestFileInfo(filepath.Join(name, entry.Name()))
			if statErr != nil {
				return statErr
			}
			batch = append(batch, info)
		}
		if len(batch) > 0 {
			if sendErr := encoder.Encode(Message{Type: MessageFileEntries, Entries: batch}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// writeGuestFile commits only after file_end; EOF, cancellation and malformed
// input discard the temporary file. Rename is atomic on the destination FS.
//
// validate -> private temp -> ready -> data... -> file_end -> close -> rename
//
//	\-> disconnect/error -> remove temp
//
// This provides visibility atomicity, not fsync durability across guest crashes.
func writeGuestFile(ctx context.Context, decoder *Decoder, encoder *Encoder, request Message) error {
	mode := os.FileMode(0o600)
	info, err := os.Lstat(request.Path)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: write destination must be a regular file, not a symlink or directory", errInvalidFileRequest)
		}
		mode = info.Mode().Perm()
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if request.Mode != nil {
		mode = os.FileMode(*request.Mode)
	}
	directory := filepath.Dir(request.Path)
	if request.Parents {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	file, err := os.CreateTemp(directory, ".kumabox-upload-")
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name()) //nolint:gosec // private temporary file created above in the owner-selected guest directory
	}()
	if err := encoder.Encode(Message{Type: MessageFileReady}); err != nil {
		return err
	}
	for {
		message, err := decoder.Decode()
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch message.Type {
		case MessageFileData:
			if len(message.Data) > streamChunkSize {
				return fmt.Errorf("%w: file chunk exceeds %d bytes", errInvalidFileRequest, streamChunkSize)
			}
			if _, err := file.Write(message.Data); err != nil {
				return err
			}
		case MessageFileEnd:
			if err := errors.Join(file.Chmod(mode), file.Close()); err != nil {
				return err
			}
			return os.Rename(file.Name(), request.Path) //nolint:gosec // rename publishes a private guest file; destination is never interpreted by the host
		default:
			return fmt.Errorf("%w: expected file_data or file_end", errInvalidFileRequest)
		}
	}
}
