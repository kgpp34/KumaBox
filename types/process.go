package types

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ProcessInfo describes an agent-managed background command, not a host VMM.
// Records and buffered output live for the guest agent's lifetime; old exited
// records may be evicted when the bounded process table fills.
type ProcessInfo struct {
	// ID is an opaque handle so a recycled guest PID never selects another command.
	ID string `json:"id"`
	// PID is diagnostic only; callers address commands using ID.
	PID int `json:"pid"`
	// Args is the original argv. Environment values are never listed.
	Args []string `json:"args"`
	// Cwd and User retain the explicit execution options, if supplied.
	Cwd  string `json:"cwd,omitempty"`
	User string `json:"user,omitempty"`
	// Running remains true until process exit and output drain complete.
	Running bool `json:"running"`
	// ExitCode is absent while running; signals use 128+signal on Linux.
	ExitCode *int `json:"exit_code,omitempty"`
	// StartedAt and ExitedAt are UTC guest timestamps.
	StartedAt time.Time  `json:"started_at"`
	ExitedAt  *time.Time `json:"exited_at,omitempty"`
	// Error reports an abnormal wait or output-drain failure, if any.
	Error string `json:"error,omitempty"`
	// OutputTruncated means some output is no longer available in the bounded buffer.
	OutputTruncated bool `json:"output_truncated,omitempty"`
}

// ValidateProcessID rejects raw PIDs and malformed opaque process handles.
func ValidateProcessID(id string) error {
	if len(id) != 32 {
		return errors.New("process ID must be a 32-character hexadecimal handle")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return fmt.Errorf("invalid process ID: %w", err)
	}
	return nil
}

// ProcessSignal is an explicitly supported guest process-group signal.
type ProcessSignal string

const (
	// ProcessSignalTERM requests graceful termination; callers choose when to escalate.
	ProcessSignalTERM ProcessSignal = "TERM"
	// ProcessSignalKILL forcibly terminates the command's process group.
	ProcessSignalKILL ProcessSignal = "KILL"
	// ProcessSignalINT interrupts the command, like an interactive Ctrl-C.
	ProcessSignalINT ProcessSignal = "INT"
	// ProcessSignalHUP delivers a hangup signal to the command.
	ProcessSignalHUP ProcessSignal = "HUP"
)

// Validate keeps host and guest signal handling on the same portable subset.
func (s ProcessSignal) Validate() error {
	switch s {
	case ProcessSignalTERM, ProcessSignalKILL, ProcessSignalINT, ProcessSignalHUP:
		return nil
	default:
		return fmt.Errorf("unsupported process signal %q; use TERM, KILL, INT or HUP", s)
	}
}

// ProcessOutputOptions resumes after the last observed output frame. Zero
// replays all output still retained by the agent. Attach is output-only.
type ProcessOutputOptions struct {
	After uint64
}

// ProcessOutputResult reports a replay position and, for a completed command,
// its exit status. Truncated explicitly reports a gap in the requested output.
type ProcessOutputResult struct {
	Cursor    uint64
	Truncated bool
	ExitCode  *int
}
