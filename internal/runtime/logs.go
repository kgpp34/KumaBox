package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// VMLogChunk is one append-only unit emitted while following logs.
type VMLogChunk struct {
	VMID    string `json:"vmId"`
	VMName  string `json:"vmName"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// LogsVM reads selected VM logs without requiring the VM to be running.
//
// Missing log files are skipped. This lets logs work consistently for created,
// failed, stopped, and deleted-after-failure states where only some streams may
// have been produced.
func (r *Runtime) LogsVM(ref string, opts LogOptions) (*VMLogs, error) {
	rec, err := r.vmReader.Inspect(ref)
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

// FollowLogsVM emits existing tail content and then appended bytes until ctx
// is cancelled. Polling deliberately handles files created after subscription,
// truncation on VM restart, and atomic file replacement without fsnotify.
func (r *Runtime) FollowLogsVM(
	ctx context.Context,
	ref string,
	opts LogOptions,
	interval time.Duration,
	emit func(VMLogChunk) error,
) error {
	if interval <= 0 {
		return fmt.Errorf("log follow interval must be positive")
	}
	if emit == nil {
		return fmt.Errorf("log follow emitter is required")
	}
	if !ValidLogSource(opts.Source) {
		return fmt.Errorf("invalid log source %q", opts.Source)
	}
	rec, err := r.vmReader.Inspect(ref)
	if err != nil {
		return err
	}
	files := make([]followedLog, 0, len(logFileNames(opts.Source)))
	for _, name := range logFileNames(opts.Source) {
		files = append(files, followedLog{name: name, path: filepath.Join(rec.LogDir, name)})
	}

	poll := func(initial bool) error {
		for i := range files {
			content, changed, err := files[i].read(initial, opts.Tail)
			if err != nil {
				return fmt.Errorf("follow log %s: %w", files[i].path, err)
			}
			if changed && content != "" {
				if err := emit(VMLogChunk{VMID: rec.ID, VMName: rec.Name, Name: files[i].name, Path: files[i].path, Content: content}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := poll(true); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := poll(false); err != nil {
				return err
			}
		}
	}
}

type followedLog struct {
	name   string
	path   string
	info   os.FileInfo
	offset int64
}

func (f *followedLog) read(initial bool, tail int) (string, bool, error) {
	file, err := os.Open(f.path) //nolint:gosec
	if errors.Is(err, os.ErrNotExist) {
		f.info = nil
		f.offset = 0
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	firstAppearance := f.info == nil
	reset := firstAppearance || !os.SameFile(f.info, info) || info.Size() < f.offset
	start := f.offset
	if reset {
		start = 0
	}
	if (initial || firstAppearance) && start == 0 && tail > 0 {
		start, err = tailOffset(file, tail)
		if err != nil {
			return "", false, err
		}
	}
	if start > info.Size() {
		start = 0
	}
	raw, err := io.ReadAll(io.NewSectionReader(file, start, info.Size()-start))
	if err != nil {
		return "", false, err
	}
	f.info = info
	f.offset = info.Size()
	return string(raw), reset || len(raw) > 0, nil
}

func tailOffset(file *os.File, tail int) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return 0, err
	}
	content := string(raw)
	trimmed := strings.TrimSuffix(content, "\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= tail {
		return 0, nil
	}
	kept := strings.Join(lines[len(lines)-tail:], "\n")
	if strings.HasSuffix(content, "\n") {
		kept += "\n"
	}
	return info.Size() - int64(len(kept)), nil
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

// LogFileNames returns the stable file order selected by source.
func LogFileNames(source string) []string {
	return append([]string(nil), logFileNames(source)...)
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
