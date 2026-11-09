package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/types"
)

// ErrCaptureRecoveryPending keeps unpublished artifacts while a monitor may
// still be writing them. Recovery must settle the operation before reclamation.
var ErrCaptureRecoveryPending = errors.New("capture recovery is pending")

// CaptureRecoverer repairs only captures whose durable intent names the exact
// owned process. Callers hold the sandbox operation lock throughout recovery.
type CaptureRecoverer interface {
	RecoverCapture(context.Context, types.SandboxID, uint64) (bool, error)
}

// CaptureAction identifies the recovery decision persisted before a VM change.
type CaptureAction string

const (
	// CaptureResume restores guest execution after an interrupted capture.
	CaptureResume CaptureAction = "resume"
	// CaptureStop finishes termination after hibernation persistence succeeds.
	CaptureStop CaptureAction = "stop"
)

// CaptureIntent survives a controller exit between pause and resume. Action
// changes to stop after hibernation persistence and before VMM termination.
type CaptureIntent struct {
	// Process fences recovery to its PID, start time, host boot and generation.
	Process Process `json:"process"`
	// Destination identifies the staging directory protected from reclamation.
	Destination string `json:"destination"`
	// Action explicitly authorizes resume or termination for this process.
	Action CaptureAction `json:"action"`
}

func (i CaptureIntent) validate() error {
	if err := i.Process.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(i.Destination) {
		return errors.New("capture destination must be absolute")
	}
	switch i.Action {
	case CaptureResume, CaptureStop:
		return nil
	default:
		return fmt.Errorf("unknown capture recovery action %q", i.Action)
	}
}

// WriteCapture atomically records the recovery decision before changing the VM.
func (p Paths) WriteCapture(intent CaptureIntent) error {
	if err := intent.validate(); err != nil {
		return err
	}
	name, err := p.runFile(intent.Process.SandboxID, "capture.json")
	if err != nil {
		return err
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return writeAtomic(name, data, 0o600)
}

// ReadCapture loads a validated intent for one sandbox. Missing files preserve
// compatibility with VMMs launched before capture journaling was introduced.
func (p Paths) ReadCapture(id types.SandboxID) (CaptureIntent, error) {
	name, err := p.runFile(id, "capture.json")
	if err != nil {
		return CaptureIntent{}, err
	}
	data, err := os.ReadFile(name) //nolint:gosec // validated runtime root and sandbox ID
	if err != nil {
		return CaptureIntent{}, err
	}
	var intent CaptureIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return intent, err
	}
	if err := intent.validate(); err != nil {
		return intent, err
	}
	if intent.Process.SandboxID != id {
		return intent, fmt.Errorf("capture intent belongs to another sandbox")
	}
	return intent, nil
}

// ClearCapture releases a settled intent. Cleanup also removes it after exit.
func (p Paths) ClearCapture(id types.SandboxID) error {
	name, err := p.runFile(id, "capture.json")
	if err != nil {
		return err
	}
	if err := os.Remove(name); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	directory, err := os.Open(filepath.Dir(name)) //nolint:gosec // managed runtime directory
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
