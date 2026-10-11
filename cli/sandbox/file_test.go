package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/types"
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
