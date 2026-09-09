package sandbox

import (
	"errors"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	t.Parallel()

	states := []State{
		StateUnknown,
		StateCreating,
		StateStopped,
		StateStarting,
		StateRunning,
		StatePaused,
		StateStopping,
		StateError,
		StateDeleting,
	}
	for _, state := range states {
		state := state
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()

			if err := state.Validate(); err != nil {
				t.Fatalf("State(%d).Validate() error = %v", state, err)
			}
			parsed, err := ParseState(state.String())
			if err != nil {
				t.Fatalf("ParseState(%q) error = %v", state.String(), err)
			}
			if parsed != state {
				t.Fatalf("ParseState(%q) = %v, want %v", state.String(), parsed, state)
			}
		})
	}
}

func TestStateUnknownIsZeroValue(t *testing.T) {
	t.Parallel()

	var state State
	if state != StateUnknown || !state.IsUnknown() {
		t.Fatalf("zero State = %v, want StateUnknown", state)
	}
}

func TestInvalidState(t *testing.T) {
	t.Parallel()

	state := State(255)
	if !errors.Is(state.Validate(), ErrInvalidState) {
		t.Fatalf("State(255).Validate() error = %v, want ErrInvalidState", state.Validate())
	}
	if _, err := ParseState("RUNNING"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("ParseState(RUNNING) error = %v, want ErrInvalidState", err)
	}
	if CanTransition(state, StateError) {
		t.Fatal("invalid source state can transition")
	}
	if err := ValidateTransition(StateRunning, state); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("ValidateTransition to invalid state error = %v, want ErrInvalidState", err)
	}
}

func TestStateTransitionMatrix(t *testing.T) {
	t.Parallel()

	states := []State{
		StateUnknown,
		StateCreating,
		StateStopped,
		StateStarting,
		StateRunning,
		StatePaused,
		StateStopping,
		StateError,
		StateDeleting,
	}
	allowed := map[State]map[State]bool{
		StateUnknown:  {StateCreating: true},
		StateCreating: {StateStopped: true, StateStarting: true, StateError: true, StateDeleting: true},
		StateStopped:  {StateStarting: true, StateDeleting: true, StateError: true},
		StateStarting: {StateRunning: true, StateError: true, StateStopping: true},
		StateRunning:  {StatePaused: true, StateStopping: true, StateError: true},
		StatePaused:   {StateRunning: true, StateStopping: true, StateError: true},
		StateStopping: {StateStopped: true, StateError: true},
		StateError:    {StateStarting: true, StateStopping: true, StateDeleting: true},
		StateDeleting: {StateError: true},
	}

	for _, source := range states {
		for _, target := range states {
			want := allowed[source][target]
			if got := CanTransition(source, target); got != want {
				t.Errorf("CanTransition(%s, %s) = %t, want %t", source, target, got, want)
			}

			err := ValidateTransition(source, target)
			if want && err != nil {
				t.Errorf("ValidateTransition(%s, %s) error = %v", source, target, err)
			}
			if !want && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("ValidateTransition(%s, %s) error = %v, want ErrInvalidTransition", source, target, err)
			}
		}
	}
}

func TestTransitionErrorSupportsErrorsAs(t *testing.T) {
	t.Parallel()

	err := ValidateTransition(StateRunning, StateStopped)
	var transitionError *TransitionError
	if !errors.As(err, &transitionError) {
		t.Fatalf("ValidateTransition error = %v, want *TransitionError", err)
	}
	if transitionError.From != StateRunning || transitionError.To != StateStopped {
		t.Fatalf("TransitionError = %#v", transitionError)
	}
}
