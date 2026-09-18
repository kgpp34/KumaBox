package vmm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

const (
	apiSocketName = "api.sock"
	vsockName     = "vsock.uds"
	processName   = "process.json"
	cmdlineName   = "cmdline"
	logName       = "vmm.log"
)

// Paths derives ephemeral runtime files and persistent VMM logs from shared roots.
type Paths struct {
	// roots is validated once so every derived path stays within its owner root.
	roots storage.Roots
}

// NewPaths validates roots without creating directories.
func NewPaths(roots storage.Roots) (Paths, error) {
	validated, err := roots.Validate()
	if err != nil {
		return Paths{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
	}
	return Paths{roots: validated}, nil
}

// Ensure creates only shared runtime and log parents. Per-sandbox directories
// are created by Prepare with private permissions immediately before launch.
func (p Paths) Ensure() error {
	return errors.Join(storage.EnsureDir(p.RunBase()), storage.EnsureDir(p.LogBase()))
}

// RunBase contains reconstructable per-sandbox VMM state.
func (p Paths) RunBase() string { return filepath.Join(p.roots.Run, "sandboxes") }

// LogBase contains durable per-sandbox VMM logs.
func (p Paths) LogBase() string { return filepath.Join(p.roots.Log, "sandboxes") }

// RunDir returns one sandbox's private runtime directory.
func (p Paths) RunDir(id types.SandboxID) (string, error) { return p.idDir(p.RunBase(), id) }

// LogDir returns one sandbox's private log directory.
func (p Paths) LogDir(id types.SandboxID) (string, error) { return p.idDir(p.LogBase(), id) }

// APISocket is the unique Cloud Hypervisor control endpoint and process marker.
func (p Paths) APISocket(id types.SandboxID) (string, error) { return p.runFile(id, apiSocketName) }

// Vsock is the private host endpoint for the future guest-agent transport.
func (p Paths) Vsock(id types.SandboxID) (string, error) { return p.runFile(id, vsockName) }

// ProcessFile stores the PID generation and host boot identity.
func (p Paths) ProcessFile(id types.SandboxID) (string, error) { return p.runFile(id, processName) }

// Cmdline stores the exact VMM invocation for diagnostics.
func (p Paths) Cmdline(id types.SandboxID) (string, error) { return p.runFile(id, cmdlineName) }

// LogFile stores stdout and stderr from the owned VMM process.
func (p Paths) LogFile(id types.SandboxID) (string, error) {
	dir, err := p.LogDir(id)
	if err != nil {
		return "", err
	}
	return storage.Join(dir, logName)
}

// Prepare creates private per-sandbox runtime and log directories.
func (p Paths) Prepare(id types.SandboxID) error {
	if err := p.Ensure(); err != nil {
		return err
	}
	for _, directory := range []func(types.SandboxID) (string, error){p.RunDir, p.LogDir} {
		path, err := directory(id)
		if err != nil {
			return err
		}
		if err := storage.EnsureDir(path); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // runtime directories intentionally require owner traversal
			return fmt.Errorf("set private directory mode on %s: %w", path, err)
		}
	}
	return nil
}

// WriteProcess atomically replaces process identity after validating every field.
func (p Paths) WriteProcess(process Process) error {
	if err := process.Validate(); err != nil {
		return err
	}
	path, err := p.ProcessFile(process.SandboxID)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(process, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(raw, '\n'), 0o600)
}

// ReadProcess decodes and validates a complete identity file.
func (p Paths) ReadProcess(id types.SandboxID) (Process, error) {
	path, err := p.ProcessFile(id)
	if err != nil {
		return Process{}, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is derived from a validated ID and root
	if err != nil {
		return Process{}, err
	}
	var process Process
	if err := json.Unmarshal(raw, &process); err != nil {
		return Process{}, fmt.Errorf("decode process identity: %w", err)
	}
	if err := process.Validate(); err != nil {
		return Process{}, fmt.Errorf("validate process identity: %w", err)
	}
	if process.SandboxID != id {
		return Process{}, errors.New("process identity belongs to another sandbox")
	}
	return process, nil
}

// WriteCmdline atomically records a diagnostic rendering before exec.
func (p Paths) WriteCmdline(id types.SandboxID, command string) error {
	path, err := p.Cmdline(id)
	if err != nil {
		return err
	}
	return writeAtomic(path, []byte(command+"\n"), 0o600)
}

// Clear removes reconstructable files after the process has been proven absent.
func (p Paths) Clear(id types.SandboxID) error {
	dir, err := p.RunDir(id)
	if err != nil {
		return err
	}
	if err := storage.CheckPath(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove VMM runtime %s: %w", dir, err)
	}
	return nil
}

func (p Paths) idDir(root string, id types.SandboxID) (string, error) {
	if _, err := types.ParseSandboxID(id.String()); err != nil {
		return "", err
	}
	return storage.Join(root, id.String())
}

func (p Paths) runFile(id types.SandboxID, name string) (string, error) {
	dir, err := p.RunDir(id)
	if err != nil {
		return "", err
	}
	return storage.Join(dir, name)
}

// writeAtomic makes a complete file visible in one rename and syncs its parent.
// Runtime identity is small, but a partial write can authorize the wrong PID.
func writeAtomic(path string, data []byte, mode os.FileMode) (returnErr error) {
	if len(data) == 0 {
		return errors.New("refuse to atomically write empty runtime data")
	}
	if err := storage.CheckPath(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := storage.EnsureDir(dir); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".runtime-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, temporary.Close())
		}
		if returnErr != nil {
			returnErr = errors.Join(returnErr, os.Remove(temporaryPath))
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir) //nolint:gosec // validated managed directory
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
