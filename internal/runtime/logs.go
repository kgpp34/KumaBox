package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// LogSourceConsole is the guest serial console stream.
	//
	// This is the default because it contains kernel, cloud-init, and login
	// output needed to debug early boot before guest-agent support exists.
	LogSourceConsole = "console"

	// LogSourceStdout is Cloud Hypervisor's stdout stream.
	LogSourceStdout = "stdout"

	// LogSourceStderr is Cloud Hypervisor's stderr stream.
	LogSourceStderr = "stderr"

	// LogSourceVMM returns both Cloud Hypervisor process streams.
	LogSourceVMM = "vmm"

	// LogSourceAll returns guest console plus VMM process streams.
	LogSourceAll = "all"
)

// LogOptions controls which VM logs are returned and how much content is read.
type LogOptions struct {
	Tail   int
	Source string
}

// VMLogFile is one log file returned by a logs request.
type VMLogFile struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// VMLogs groups all log files selected for a VM.
type VMLogs struct {
	VMID  string      `json:"vmId"`
	Name  string      `json:"name"`
	Files []VMLogFile `json:"files"`
}

// LogsVM reads selected VM logs without requiring the VM to be running.
//
// Missing log files are skipped. This lets logs work consistently for created,
// failed, stopped, and deleted-after-failure states where only some streams may
// have been produced.
func (r *Runtime) LogsVM(ref string, opts LogOptions) (*VMLogs, error) {
	rec, err := r.store.Inspect(ref)
	if err != nil {
		return nil, err
	}

	logs := &VMLogs{
		VMID: rec.ID,
		Name: rec.Name,
	}
	for _, name := range logFileNames(opts.Source) {
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

func logFileNames(source string) []string {
	if source == "" {
		source = LogSourceConsole
	}
	switch source {
	case LogSourceConsole:
		return []string{"console.log"}
	case LogSourceStdout:
		return []string{"cloud-hypervisor.stdout.log"}
	case LogSourceStderr:
		return []string{"cloud-hypervisor.stderr.log"}
	case LogSourceVMM:
		return []string{"cloud-hypervisor.stdout.log", "cloud-hypervisor.stderr.log"}
	case LogSourceAll:
		return []string{"console.log", "cloud-hypervisor.stdout.log", "cloud-hypervisor.stderr.log"}
	default:
		return nil
	}
}

// ValidLogSource reports whether source is accepted by LogsVM.
//
// An empty source is valid and resolves to the guest console.
func ValidLogSource(source string) bool {
	return source == "" || len(logFileNames(source)) > 0
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
