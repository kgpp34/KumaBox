package vmm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

const (
	logFollowInterval = 100 * time.Millisecond
	logHeadSize       = 256
)

// LogOptions controls the backend-independent VMM log stream.
type LogOptions struct {
	// Tail starts output at the last N lines. Zero streams the complete file.
	Tail int
	// Follow waits for appended data and survives VMM log truncation or replacement.
	Follow bool
}

// Validate rejects options that have no useful command-line meaning.
func (o LogOptions) Validate() error {
	if o.Tail < 0 {
		return errors.New("log tail must not be negative")
	}
	return nil
}

// Logs streams one backend-owned log without exposing its host path to core.
// Follow polling deliberately stays synchronous: cancellation has one owner and
// cannot leak a watcher goroutine after a CLI invocation exits.
//
//	open -> optional tail -> copy available bytes
//	                           |
//	                follow: poll -> append / rewind / reopen
func (p Paths) Logs(ctx context.Context, id types.SandboxID, options LogOptions, output io.Writer) (returnErr error) {
	if err := options.Validate(); err != nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	if output == nil {
		return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, errors.New("log output is required"))
	}
	path, err := p.LogFile(id)
	if err != nil {
		return err
	}
	current, err := openLog(path)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, current.Close()) }()

	if options.Tail > 0 {
		if err := seekLastLines(current, options.Tail); err != nil {
			return fmt.Errorf("seek VMM log tail: %w", err)
		}
	}
	if err := copyLog(ctx, output, current); err != nil {
		if options.Follow && ctx.Err() != nil {
			return nil
		}
		return err
	}
	if !options.Follow {
		return nil
	}

	offset, err := current.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("locate VMM log offset: %w", err)
	}
	signature, err := logSignature(current, 0)
	if err != nil {
		return fmt.Errorf("read VMM log signature: %w", err)
	}
	ticker := time.NewTicker(logFollowInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		pathInfo, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			// Successful rm owns log deletion. A follower that already opened the
			// log completes cleanly instead of waiting on an unlinked inode.
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat VMM log: %w", err)
		}
		openInfo, err := current.Stat()
		if err != nil {
			return fmt.Errorf("stat open VMM log: %w", err)
		}
		if !os.SameFile(pathInfo, openInfo) {
			next, err := openLog(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if err := current.Close(); err != nil {
				_ = next.Close()
				return fmt.Errorf("close replaced VMM log: %w", err)
			}
			current = next
			offset = 0
			signature = nil
		}

		newSignature, err := logSignature(current, len(signature))
		if err != nil {
			return fmt.Errorf("read VMM log signature: %w", err)
		}
		if pathInfo.Size() < offset || len(signature) > 0 && !bytes.Equal(newSignature, signature) {
			if _, err := current.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("rewind truncated VMM log: %w", err)
			}
			signature, err = logSignature(current, 0)
			if err != nil {
				return fmt.Errorf("read truncated VMM log signature: %w", err)
			}
		}
		if err := copyLog(ctx, output, current); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		offset, err = current.Seek(0, io.SeekCurrent)
		if err != nil {
			return fmt.Errorf("locate VMM log offset: %w", err)
		}
		if len(signature) == 0 && offset > 0 {
			signature, err = logSignature(current, 0)
			if err != nil {
				return fmt.Errorf("read VMM log signature: %w", err)
			}
		}
	}
}

// RemoveLogs removes all persistent log artifacts owned by one backend.
func (p Paths) RemoveLogs(ctx context.Context, id types.SandboxID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := p.LogDir(id)
	if err != nil {
		return err
	}
	if err := storage.CheckPath(directory); err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove VMM log directory %s: %w", directory, err)
	}
	return nil
}

func openLog(path string) (*os.File, error) {
	file, err := os.Open(path) //nolint:gosec // path is derived from validated managed roots and sandbox identity
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("VMM log is unavailable; the sandbox may not have been started yet: %w", err))
	}
	if err != nil {
		return nil, errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, fmt.Errorf("open VMM log: %w", err))
	}
	return file, nil
}

// seekLastLines treats a final newline as a terminator rather than an empty
// extra line, so --tail 1 on "one\ntwo\n" starts at "two".
func seekLastLines(file *os.File, count int) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	const chunkSize = 4096
	buffer := make([]byte, chunkSize)
	position, found := size, 0
	for position > 0 {
		readSize := min(int64(chunkSize), position)
		position -= readSize
		if _, err := file.ReadAt(buffer[:readSize], position); err != nil {
			return err
		}
		for index := readSize - 1; index >= 0; index-- {
			if buffer[index] != '\n' || position+index == size-1 {
				continue
			}
			found++
			if found == count {
				_, err := file.Seek(position+index+1, io.SeekStart)
				return err
			}
		}
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

func copyLog(ctx context.Context, output io.Writer, file *os.File) error {
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			written, writeErr := output.Write(buffer[:read])
			if writeErr != nil {
				return fmt.Errorf("write VMM log: %w", writeErr)
			}
			if written != read {
				return fmt.Errorf("write VMM log: %w", io.ErrShortWrite)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read VMM log: %w", readErr)
		}
	}
}

// logSignature reads a stable prefix without changing the stream offset. When
// width is nonzero, the original width is retained as the comparison contract.
func logSignature(file *os.File, width int) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if width == 0 {
		width = min(logHeadSize, int(info.Size()))
	}
	if width == 0 {
		return nil, nil
	}
	signature := make([]byte, width)
	read, err := file.ReadAt(signature, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return signature[:read], nil
}
