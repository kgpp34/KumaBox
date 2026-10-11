package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kumabox/kumabox/types"
)

func TestGuestFileBinaryRoundTripAndMutations(t *testing.T) {
	server := processServer(t)
	connect := func() net.Conn { return processConnection(t, server) }
	name := filepath.Join(t.TempDir(), "workspace", "binary")
	payload := bytes.Repeat([]byte{0, 1, 255, '\n'}, streamChunkSize+1)
	mode := uint32(0o640)
	if err := WriteFile(t.Context(), connect(), name, types.FileWriteOptions{Parents: true, Mode: &mode}, io.NopCloser(bytes.NewReader(payload))); err != nil {
		t.Fatal(err)
	}
	info, err := StatFile(t.Context(), connect(), name)
	if err != nil || info.Size != int64(len(payload)) || info.Mode != mode || info.Kind != "file" {
		t.Fatalf("stat = %+v, %v", info, err)
	}
	var received bytes.Buffer
	if err := ReadFile(t.Context(), connect(), name, &received); err != nil || !bytes.Equal(payload, received.Bytes()) {
		t.Fatalf("binary round trip = %d bytes, %v", received.Len(), err)
	}
	if err := WriteFile(t.Context(), connect(), name, types.FileWriteOptions{}, io.NopCloser(strings.NewReader("replaced"))); err != nil {
		t.Fatal(err)
	}
	info, err = StatFile(t.Context(), connect(), name)
	if err != nil || info.Mode != mode || info.Size != 8 {
		t.Fatalf("preserved mode = %+v, %v", info, err)
	}
	renamed := name + ".moved"
	if err := RenameFile(t.Context(), connect(), name, renamed); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFile(t.Context(), connect(), filepath.Dir(name), false); err == nil {
		t.Fatal("removed a nonempty directory without recursive opt-in")
	}
	if err := RemoveFile(t.Context(), connect(), filepath.Dir(name), true); err != nil {
		t.Fatal(err)
	}
	_, err = StatFile(t.Context(), connect(), renamed)
	assertFileError(t, err, "NOT_FOUND")
}

func assertFileError(t *testing.T, err error, code string) {
	t.Helper()
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != code {
		t.Fatalf("expected remote %s, got %v", code, err)
	}
}

func TestGuestFileSymlinksAndNonregularReads(t *testing.T) {
	server := processServer(t)
	connect := func() net.Conn { return processConnection(t, server) }
	directory := t.TempDir()
	target, link := filepath.Join(directory, "target"), filepath.Join(directory, "link")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	info, err := StatFile(t.Context(), connect(), link)
	if err != nil || info.Kind != "symlink" || info.LinkTarget != target {
		t.Fatalf("lstat = %+v, %v", info, err)
	}
	var content bytes.Buffer
	if err := ReadFile(t.Context(), connect(), link, &content); err != nil || content.String() != "original" {
		t.Fatalf("read symlink = %q, %v", content.String(), err)
	}
	err = WriteFile(t.Context(), connect(), link, types.FileWriteOptions{}, io.NopCloser(strings.NewReader("changed")))
	assertFileError(t, err, "INVALID_ARGUMENT")
	if err := RemoveFile(t.Context(), connect(), link, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original" {
		t.Fatalf("symlink operation changed target: %q, %v", data, err)
	}
	fifo := filepath.Join(directory, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for _, name := range []string{directory, fifo, "/dev/null"} {
		err := ReadFile(ctx, connect(), name, io.Discard)
		assertFileError(t, err, "INVALID_ARGUMENT")
	}
}

func TestGuestDirectoryStreamsMultipleBatches(t *testing.T) {
	server := processServer(t)
	directory := filepath.Join(t.TempDir(), "nested", "entries")
	if err := MakeDirectory(t.Context(), processConnection(t, server), directory, true); err != nil {
		t.Fatal(err)
	}
	for index := range 257 {
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprint(index)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool)
	err := ListFiles(t.Context(), processConnection(t, server), directory, func(info types.FileInfo) error {
		if seen[info.Name] || info.Path != filepath.Join(directory, info.Name) {
			return fmt.Errorf("invalid or duplicate entry %+v", info)
		}
		seen[info.Name] = true
		return nil
	})
	if err != nil || len(seen) != 257 {
		t.Fatalf("listing = %d entries, %v", len(seen), err)
	}
	visitorErr := errors.New("stop listing")
	err = ListFiles(t.Context(), processConnection(t, server), directory, func(types.FileInfo) error { return visitorErr })
	if !errors.Is(err, visitorErr) {
		t.Fatalf("visitor error = %v", err)
	}
}

func TestGuestUploadAbortPreservesOriginal(t *testing.T) {
	for _, failure := range []string{"disconnect", "malformed", "oversized"} {
		t.Run(failure, func(t *testing.T) {
			server := processServer(t)
			directory := t.TempDir()
			name := filepath.Join(directory, "target")
			if err := os.WriteFile(name, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			host, guest := net.Pipe()
			done := make(chan struct{})
			go func() { defer close(done); server.handle(t.Context(), guest) }()
			encoder, decoder := NewEncoder(host), NewDecoder(host)
			if err := encoder.Encode(Message{Type: MessageFileWrite, Path: name}); err != nil {
				t.Fatal(err)
			}
			ready, err := decoder.Decode()
			if err != nil || ready.Type != MessageFileReady {
				t.Fatalf("ready = %+v, %v", ready, err)
			}
			if err := encoder.Encode(Message{Type: MessageFileData, Data: []byte("partial")}); err != nil {
				t.Fatal(err)
			}
			if failure != "disconnect" {
				message := Message{Type: MessageExec}
				if failure == "oversized" {
					message = Message{Type: MessageFileData, Data: make([]byte, streamChunkSize+1)}
				}
				if err := encoder.Encode(message); err != nil {
					t.Fatal(err)
				}
				response, err := decoder.Decode()
				if err != nil || response.Type != MessageError || response.Code != "INVALID_ARGUMENT" {
					t.Fatalf("rejected upload = %+v, %v", response, err)
				}
			}
			_ = host.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("upload did not exit on disconnect")
			}
			data, err := os.ReadFile(name)
			if err != nil || string(data) != "original" {
				t.Fatalf("aborted upload changed target: %q, %v", data, err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary file leaked: %v, %v", entries, err)
			}
		})
	}
}

func TestGuestUploadCancellationUnblocksInput(t *testing.T) {
	server := processServer(t)
	name := filepath.Join(t.TempDir(), "target")
	input, writer := io.Pipe()
	defer writer.Close() //nolint:errcheck
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- WriteFile(ctx, processConnection(t, server), name, types.FileWriteOptions{}, input) }()
	if _, err := writer.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled upload leaked its input reader")
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled upload published target: %v", err)
	}
}

func TestGuestUploadRemoteErrorInterruptsProducer(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { _ = host.Close(); _ = guest.Close() })
	go func() {
		decoder, encoder := NewDecoder(guest), NewEncoder(guest)
		_, _ = decoder.Decode()
		_ = encoder.Encode(Message{Type: MessageFileReady})
		_, _ = decoder.Decode()
		fileFailure(encoder, errors.New("disk is full"))
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := WriteFile(ctx, host, "/target", types.FileWriteOptions{}, io.NopCloser(bytes.NewReader(make([]byte, streamChunkSize*4))))
	assertFileError(t, err, "IO_ERROR")
}

func TestGuestFileOldAgentFailsClosed(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { _ = host.Close(); _ = guest.Close() })
	go func() {
		_, _ = NewDecoder(guest).Decode()
		_ = NewEncoder(guest).sendError("unknown operation")
	}()
	err := WriteFile(t.Context(), host, "/target", types.FileWriteOptions{}, io.NopCloser(strings.NewReader("data")))
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("old agent silently accepted a write: %v", err)
	}
}

type failedFileInput struct{ err error }

func (input failedFileInput) Read([]byte) (int, error) { return 0, input.err }

func TestGuestUploadInputFailureDoesNotCommit(t *testing.T) {
	server := processServer(t)
	directory := t.TempDir()
	name := filepath.Join(directory, "target")
	if err := os.WriteFile(name, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("source read failed")
	input := io.NopCloser(io.MultiReader(strings.NewReader("partial"), failedFileInput{err: failure}))
	err := WriteFile(t.Context(), processConnection(t, server), name, types.FileWriteOptions{}, input)
	if !errors.Is(err, failure) {
		t.Fatalf("input error = %v", err)
	}
	data, err := os.ReadFile(name)
	if err != nil || string(data) != "original" {
		t.Fatalf("input failure changed target: %q, %v", data, err)
	}
}
