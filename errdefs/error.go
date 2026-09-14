package errdefs

import (
	"errors"
	"fmt"
)

// Error carries stable classification and diagnostic context across layers.
type Error struct {
	Class     Class
	Code      Code
	Operation string
	Entity    string
	Phase     string
	Committed bool
	Retry     bool
	Action    string
	Cause     error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := string(e.Code)
	if e.Operation != "" {
		message = e.Operation + ": " + message
	}
	if e.Entity != "" {
		message += " (" + e.Entity + ")"
	}
	if e.Phase != "" {
		message += " at " + e.Phase
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	if e.Action != "" {
		message += "; " + e.Action
	}
	return message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// New classifies cause at its producing boundary.
func New(class Class, code Code, cause error) *Error {
	if cause == nil {
		cause = errors.New(string(code))
	}
	return &Error{Class: class, Code: code, Cause: cause}
}

// Context adds operation context without changing an existing classification.
func Context(err error, operation, entity, phase, action string, committed bool) error {
	if err == nil {
		return nil
	}
	var classified *Error
	if errors.As(err, &classified) {
		copy := *classified
		copy.Operation = first(operation, copy.Operation)
		copy.Entity = first(entity, copy.Entity)
		copy.Phase = first(phase, copy.Phase)
		copy.Action = first(action, copy.Action)
		copy.Committed = committed || copy.Committed
		copy.Cause = fmt.Errorf("%w", err)
		return &copy
	}
	return &Error{
		Class: ClassInternal, Code: CodeInternal, Operation: operation,
		Entity: entity, Phase: phase, Committed: committed, Action: action,
		Cause: err,
	}
}

// CodeOf returns the stable code in err's unwrap chain.
func CodeOf(err error) (Code, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func first(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
