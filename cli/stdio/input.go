// Package stdio owns cancelable CLI input without closing inherited stdin.
package stdio

import (
	"errors"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Input duplicates inherited file descriptors into Go's poller so cancellation
// can interrupt an idle pipe. Other ReadClosers retain their existing ownership.
func Input(reader io.Reader) (io.ReadCloser, error) {
	if file, ok := reader.(*os.File); ok {
		return pollableFileInput(file)
	}
	if closer, ok := reader.(io.ReadCloser); ok {
		return closer, nil
	}
	return io.NopCloser(reader), nil
}

// uploadInput owns a pollable duplicate of inherited stdin. Unlike an os.File
// created around a blocking inherited fd, Close can interrupt its pending Read.
// Descriptor flags are shared with stdin and are restored after the read stops.
type uploadInput struct {
	*os.File
	once     sync.Once
	restore  func() error
	closeErr error
}

func (input *uploadInput) Close() error {
	input.once.Do(func() {
		input.closeErr = errors.Join(input.File.Close(), input.restore())
	})
	return input.closeErr
}

func pollableFileInput(source *os.File) (io.ReadCloser, error) {
	raw, err := source.SyscallConn()
	if err != nil {
		return nil, err
	}
	descriptor, flags := -1, 0
	var controlErr error
	err = raw.Control(func(fd uintptr) {
		flags, controlErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		if controlErr == nil {
			descriptor, controlErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
		}
	})
	if err := errors.Join(err, controlErr); err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(descriptor, true); err != nil {
		return nil, errors.Join(err, unix.Close(descriptor))
	}
	input := &uploadInput{File: os.NewFile(uintptr(descriptor), source.Name())}
	input.restore = func() error {
		var restoreErr error
		err := raw.Control(func(fd uintptr) { _, restoreErr = unix.FcntlInt(fd, unix.F_SETFL, flags) })
		return errors.Join(err, restoreErr)
	}
	return input, nil
}
