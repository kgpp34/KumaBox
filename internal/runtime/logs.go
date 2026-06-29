package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type LogOptions struct {
	Tail int
}

type VMLogFile struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

type VMLogs struct {
	VMID  string      `json:"vmId"`
	Name  string      `json:"name"`
	Files []VMLogFile `json:"files"`
}

func (r *Runtime) LogsVM(ref string, opts LogOptions) (*VMLogs, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}

	logs := &VMLogs{
		VMID: rec.ID,
		Name: rec.Name,
	}
	for _, name := range []string{
		"cloud-hypervisor.stdout.log",
		"cloud-hypervisor.stderr.log",
		"console.log",
	} {
		path := filepath.Join(rec.LogDir, name)
		content, err := readLogTail(path, opts.Tail)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read log %s: %w", path, err)
		}
		logs.Files = append(logs.Files, VMLogFile{
			Name:    name,
			Path:    path,
			Content: content,
		})
	}
	return logs, nil
}

func readLogTail(path string, tail int) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return "", err
	}
	content := string(raw)
	if tail <= 0 {
		return content, nil
	}

	lines := strings.SplitAfter(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, ""), nil
}
