package sandbox

import (
	"errors"
	"fmt"
)

// State is the durable lifecycle state of a sandbox.
type State uint8

const (
	StateUnknown State = iota
	StateCreating
	StateStopped
	StateStarting
	StateRunning
	StatePaused
	StateStopping
	StateError
	StateDeleting
)

// ErrInvalidState identifies an unknown numeric or textual sandbox state.
var ErrInvalidState = errors.New("invalid sandbox state")

// ErrInvalidTransition identifies a transition not present in the sandbox
// lifecycle state machine.
var ErrInvalidTransition = errors.New("invalid sandbox state transition")

// ParseState parses a canonical sandbox state.
func ParseState(value string) (State, error) {
	switch value {
	case "unknown":
		return StateUnknown, nil
	case "creating":
		return StateCreating, nil
	case "stopped":
		return StateStopped, nil
	case "starting":
		return StateStarting, nil
	case "running":
		return StateRunning, nil
	case "paused":
		return StatePaused, nil
	case "stopping":
		return StateStopping, nil
	case "error":
		return StateError, nil
	case "deleting":
		return StateDeleting, nil
	default:
		return StateUnknown, fmt.Errorf("%w: %q", ErrInvalidState, value)
	}
}

// String returns the canonical state name.
func (state State) String() string {
	switch state {
	case StateUnknown:
		return "unknown"
	case StateCreating:
		return "creating"
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	case StateStopping:
		return "stopping"
	case StateError:
		return "error"
	case StateDeleting:
		return "deleting"
	default:
		return fmt.Sprintf("State(%d)", uint8(state))
	}
}

// IsUnknown reports whether state is the zero-value state.
func (state State) IsUnknown() bool {
	return state == StateUnknown
}

// Validate checks that state is a defined lifecycle state. StateUnknown is a
// defined value; operation-specific validation decides where it is permitted.
func (state State) Validate() error {
	if state <= StateDeleting {
		return nil
	}
	return fmt.Errorf("%w: %d", ErrInvalidState, state)
}

// CanTransition reports whether the lifecycle state machine permits a direct
// transition from source to target. Successful deletion removes the durable
// record, so it is not represented by an additional state.
func CanTransition(source, target State) bool {
	if source.Validate() != nil || target.Validate() != nil {
		return false
	}

	switch source {
	case StateUnknown:
		return target == StateCreating
	case StateCreating:
		return target == StateStopped || target == StateStarting || target == StateError || target == StateDeleting
	case StateStopped:
		return target == StateStarting || target == StateDeleting || target == StateError
	case StateStarting:
		return target == StateRunning || target == StateError || target == StateStopping
	case StateRunning:
		return target == StatePaused || target == StateStopping || target == StateError
	case StatePaused:
		return target == StateRunning || target == StateStopping || target == StateError
	case StateStopping:
		return target == StateStopped || target == StateError
	case StateError:
		return target == StateStarting || target == StateStopping || target == StateDeleting
	case StateDeleting:
		return target == StateError
	default:
		return false
	}
}

// ValidateTransition returns a TransitionError when a direct state transition
// is not permitted.
func ValidateTransition(source, target State) error {
	if err := source.Validate(); err != nil {
		return fmt.Errorf("validate source state: %w", err)
	}
	if err := target.Validate(); err != nil {
		return fmt.Errorf("validate target state: %w", err)
	}
	if !CanTransition(source, target) {
		return &TransitionError{From: source, To: target}
	}
	return nil
}

// TransitionError describes a rejected direct lifecycle transition.
type TransitionError struct {
	From State
	To   State
}

func (err *TransitionError) Error() string {
	return fmt.Sprintf("%s: %s -> %s", ErrInvalidTransition, err.From, err.To)
}

// Unwrap supports errors.Is with ErrInvalidTransition.
func (err *TransitionError) Unwrap() error {
	return ErrInvalidTransition
}
