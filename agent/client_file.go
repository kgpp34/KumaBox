package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kumabox/kumabox/types"
)

// StatFile describes the final path entry without dereferencing a symlink.
func StatFile(ctx context.Context, connection io.ReadWriteCloser, path string) (types.FileInfo, error) {
	var result *types.FileInfo
	err := fileRequest(ctx, connection, Message{Type: MessageFileStat, Path: path}, func(message Message) error {
		if message.Type != MessageFileInfo || message.File == nil || result != nil {
			return errors.New("invalid file stat response")
		}
		result = message.File
		return nil
	})
	if err != nil {
		return types.FileInfo{}, err
	}
	if result == nil {
		return types.FileInfo{}, errors.New("guest agent omitted file metadata")
	}
	return *result, nil
}

// ListFiles visits entries incrementally in filesystem order. A visitor error
// stops the stream; an error after some entries means the listing is incomplete.
func ListFiles(ctx context.Context, connection io.ReadWriteCloser, path string, visit func(types.FileInfo) error) error {
	if visit == nil {
		return errors.New("file listing requires a visitor")
	}
	return fileRequest(ctx, connection, Message{Type: MessageFileList, Path: path}, func(message Message) error {
		if message.Type != MessageFileEntries || len(message.Entries) > fileListBatchSize {
			return errors.New("invalid file listing response")
		}
		for _, info := range message.Entries {
			if err := visit(info); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReadFile streams a regular file, following symlinks. Output may be partial
// on error; callers writing a local destination should stage it themselves.
func ReadFile(ctx context.Context, connection io.ReadWriteCloser, path string, output io.Writer) error {
	if output == nil {
		return errors.New("file read requires an output writer")
	}
	return fileRequest(ctx, connection, Message{Type: MessageFileRead, Path: path}, func(message Message) error {
		if message.Type != MessageFileData || len(message.Data) > streamChunkSize {
			return errors.New("invalid file data response")
		}
		n, err := output.Write(message.Data)
		if err == nil && n != len(message.Data) {
			err = io.ErrShortWrite
		}
		return err
	})
}

// MakeDirectory creates a guest directory with 0700 permissions (subject to
// umask). Parents makes existing directories succeed and creates missing ones.
func MakeDirectory(ctx context.Context, connection io.ReadWriteCloser, path string, parents bool) error {
	return fileRequest(ctx, connection, Message{Type: MessageFileMkdir, Path: path, Parents: parents}, nil)
}

// RemoveFile removes an entry, or recursively removes a tree when requested.
// A final symlink is removed itself; its target is never recursively removed.
func RemoveFile(ctx context.Context, connection io.ReadWriteCloser, path string, recursive bool) error {
	return fileRequest(ctx, connection, Message{Type: MessageFileRemove, Path: path, Recursive: recursive}, nil)
}

// RenameFile uses guest filesystem rename semantics: existing targets may be
// replaced, and cross-filesystem renames fail without falling back to copying.
func RenameFile(ctx context.Context, connection io.ReadWriteCloser, from, to string) error {
	return fileRequest(ctx, connection, Message{Type: MessageFileRename, Path: from, Destination: to}, nil)
}

func fileRequest(ctx context.Context, connection io.ReadWriteCloser, request Message, receive func(Message) error) error {
	if err := validateFileRequest(request); err != nil {
		return err
	}
	return exchange(ctx, connection, request, func(message Message) error {
		if message.Type == MessageDone {
			return nil
		}
		if receive == nil {
			return fmt.Errorf("unexpected file response %q", message.Type)
		}
		return receive(message)
	})
}

// WriteFile atomically replaces a regular guest file. It owns input and the
// transport after validation, closing both on completion or cancellation.
// Input.Close must unblock Read; regular files and io.PipeReader satisfy this.
// A lost completion response is ambiguous: read/stat before retrying a write.
func WriteFile(ctx context.Context, connection io.ReadWriteCloser, path string, options types.FileWriteOptions, input io.ReadCloser) error {
	request := Message{Type: MessageFileWrite, Path: path, Mode: options.Mode, Parents: options.Parents}
	if err := validateFileRequest(request); err != nil {
		return err
	}
	if input == nil {
		return errors.New("file write requires an input reader")
	}
	closeIO := func() { _ = connection.Close(); _ = input.Close() }
	stop := context.AfterFunc(ctx, closeIO)
	defer stop()
	defer closeIO()
	if err := ctx.Err(); err != nil {
		return err
	}
	encoder, decoder := NewEncoder(connection), NewDecoder(connection)
	if err := encoder.Encode(request); err != nil {
		return fileTransferError(ctx, err)
	}
	ready, err := decoder.Decode()
	if err != nil {
		return fileTransferError(ctx, err)
	}
	if ready.Type == MessageError {
		return &RemoteError{Code: ready.Code, Message: ready.Message}
	}
	if ready.Type != MessageFileReady {
		return errors.New("guest agent omitted upload readiness")
	}
	// Read responses concurrently: a guest disk error must interrupt a blocked
	// upload instead of deadlocking both peers while each waits to write.
	sent := make(chan error, 1)
	go func() {
		err := sendFile(encoder, input)
		if err != nil {
			_ = connection.Close()
		}
		sent <- err
	}()
	response, receiveErr := decoder.Decode()
	closeIO()
	sendErr := <-sent
	if err := ctx.Err(); err != nil {
		return err
	}
	if receiveErr == nil && response.Type == MessageError {
		return &RemoteError{Code: response.Code, Message: response.Message}
	}
	if sendErr != nil {
		return fmt.Errorf("send guest file: %w", sendErr)
	}
	if receiveErr != nil {
		return fmt.Errorf("receive guest file completion: %w", receiveErr)
	}
	if response.Type != MessageDone {
		return fmt.Errorf("unexpected file completion %q", response.Type)
	}
	return nil
}

func fileTransferError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func sendFile(encoder *Encoder, input io.Reader) error {
	buffer := make([]byte, streamChunkSize)
	emptyReads := 0
	for {
		n, err := input.Read(buffer)
		if n == 0 && err == nil {
			emptyReads++
			if emptyReads == 100 {
				return io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
		if n > 0 {
			if sendErr := encoder.Encode(Message{Type: MessageFileData, Data: buffer[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return encoder.Encode(Message{Type: MessageFileEnd})
		}
		if err != nil {
			return err
		}
	}
}
