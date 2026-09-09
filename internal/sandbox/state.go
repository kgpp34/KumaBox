package sandbox

import (
	"errors"
	"fmt"
)

// State is the durable lifecycle state of a sandbox. Its zero value is
// StateUnknown.
type State string

const (
	StateUnknown  State = ""
	StateCreating State = "creating"
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StatePaused   State = "paused"
	StateStopping State = "stopping"
	StateError    State = "error"
	StateDeleting State = "deleting"
)

var (
	// ErrInvalidState identifies an unknown textual sandbox state.
	ErrInvalidState = errors.New("invalid sandbox state")
	// ErrInvalidTransition identifies a transition not present in the sandbox
	// lifecycle state machine.
	ErrInvalidTransition = errors.New("invalid sandbox state transition")
)

// ParseState parses and validates a state received at a module boundary.
func ParseState(value string) (State, error) {
	state := State(value)
	if err := state.Validate(); err != nil {
		return StateUnknown, err
	}
	return state, nil
}

// IsUnknown reports whether state is the zero-value state.
func (state State) IsUnknown() bool {
	return state == StateUnknown
}

// Validate checks that state is a defined lifecycle state. StateUnknown is a
// defined value; operation-specific validation decides where it is permitted.
func (state State) Validate() error {
	switch state {
	case StateUnknown,
		StateCreating,
		StateStopped,
		StateStarting,
		StateRunning,
		StatePaused,
		StateStopping,
		StateError,
		StateDeleting:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidState, state)
	}
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
	return fmt.Sprintf("%s: %q -> %q", ErrInvalidTransition, err.From, err.To)
}

// Unwrap supports errors.Is with ErrInvalidTransition.
func (err *TransitionError) Unwrap() error {
	return ErrInvalidTransition
}
