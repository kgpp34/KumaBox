package sandbox

import (
	"errors"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state State
	}{
		{name: "unknown", state: StateUnknown},
		{name: "creating", state: StateCreating},
		{name: "stopped", state: StateStopped},
		{name: "starting", state: StateStarting},
		{name: "running", state: StateRunning},
		{name: "paused", state: StatePaused},
		{name: "stopping", state: StateStopping},
		{name: "error", state: StateError},
		{name: "deleting", state: StateDeleting},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if err := test.state.Validate(); err != nil {
				t.Fatalf("State(%q).Validate() error = %v", test.state, err)
			}
			parsed, err := ParseState(string(test.state))
			if err != nil {
				t.Fatalf("ParseState(%q) error = %v", test.state, err)
			}
			if parsed != test.state {
				t.Fatalf("ParseState(%q) = %q, want %q", test.state, parsed, test.state)
			}
		})
	}
}

func TestStateUnknownIsZeroValue(t *testing.T) {
	t.Parallel()

	var state State
	if state != StateUnknown || !state.IsUnknown() {
		t.Fatalf("zero State = %q, want StateUnknown", state)
	}
}

func TestInvalidState(t *testing.T) {
	t.Parallel()

	state := State("booting")
	if !errors.Is(state.Validate(), ErrInvalidState) {
		t.Fatalf("State(booting).Validate() error = %v, want ErrInvalidState", state.Validate())
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
				t.Errorf("CanTransition(%q, %q) = %t, want %t", source, target, got, want)
			}

			err := ValidateTransition(source, target)
			if want && err != nil {
				t.Errorf("ValidateTransition(%q, %q) error = %v", source, target, err)
			}
			if !want && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf(
					"ValidateTransition(%q, %q) error = %v, want ErrInvalidTransition",
					source,
					target,
					err,
				)
			}
		}
	}
}
