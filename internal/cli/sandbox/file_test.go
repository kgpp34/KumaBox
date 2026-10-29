package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/types"
)

func TestFileCLIRejectsInvalidPathsBeforeOpeningService(t *testing.T) {
	for _, args := range [][]string{
		{"read", "box", "relative"},
		{"write", "box", "/"},
		{"rm", "box", "/", "--recursive"},
		{"mv", "box", "/tmp/source", "/"},
		{"write", "box", "/tmp/dest", "--mode", "1000"},
		{"write", "box", "/tmp/dest", "--mode", "bad"},
		{"mkdir", "box", "/tmp/../etc"},
	} {
		command := NewFileCommand(func() config.Config {
			t.Fatal("invalid arguments opened application services")
			return config.Config{}
		})
		command.SetArgs(args)
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		if err := command.ExecuteContext(t.Context()); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestFileUploadInheritedStdinCanBeCancelled(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close() //nolint:errcheck
	defer writer.Close() //nolint:errcheck
	// Fd returns a blocking descriptor, matching stdin inherited from a shell.
	fd := reader.Fd()
	before, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	command := NewFileCommand(nil)
	command.SetIn(reader)
	input, err := fileInput(command, "-")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close() //nolint:errcheck
	deadline, ok := input.(interface{ SetReadDeadline(time.Time) error })
	if !ok {
		t.Fatal("inherited stdin has no cancellable read")
	}
	if err := deadline.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("inherited stdin still performs blocking syscalls: %v", err)
	}
	_, err = input.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read deadline = %v", err)
	}
	if err := deadline.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, input); done <- err }()
	closed := make(chan error, 1)
	go func() { closed <- input.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = writer.Close()
		t.Fatal("close did not cancel inherited stdin")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled stdin reader leaked")
	}
	after, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil || after != before {
		t.Fatalf("stdin flags not restored: before=%d after=%d err=%v", before, after, err)
	}
}

func TestFileListOutputEscapesNamesAndReportsPartialFailure(t *testing.T) {
	entry := types.FileInfo{Name: "line\nwith\ttabs", Path: "/tmp/file", Kind: "file", Mode: 0o600}
	partial := errors.New("directory changed while listing")
	list := func(visit func(types.FileInfo) error) error {
		if err := visit(entry); err != nil {
			return err
		}
		return partial
	}
	for _, asJSON := range []bool{false, true} {
		var output bytes.Buffer
		err := printFileList(&output, asJSON, list)
		if !errors.Is(err, partial) {
			t.Fatalf("partial listing error = %v", err)
		}
		if asJSON {
			var entries []types.FileInfo
			if err := json.Unmarshal(output.Bytes(), &entries); err != nil || len(entries) != 1 || entries[0].Name != entry.Name {
				t.Fatalf("JSON listing = %q, %v", output.String(), err)
			}
		} else if !strings.Contains(output.String(), "MODE") || !strings.Contains(output.String(), `"line\nwith\ttabs"`) {
			t.Fatalf("table listing = %q", output.String())
		}
	}
}
