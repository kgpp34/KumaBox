// Package errdefs carries stable failure codes, handling classes, and operation
// context across module boundaries while preserving the original error chain.
package errdefs

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable failure code.
type Code string

const (
	// CodeNotFound indicates the requested entity or name is absent.
	CodeNotFound Code = "NOT_FOUND"
	// CodeNameTaken indicates a name is already bound to conflicting state.
	CodeNameTaken Code = "NAME_TAKEN"
	// CodeStateConflict indicates a generation or lifecycle precondition changed.
	CodeStateConflict Code = "STATE_CONFLICT"
	// CodeInvalidArgument indicates an argument or option violates the operation contract.
	CodeInvalidArgument Code = "INVALID_ARGUMENT"
	// CodeHostIncompatible indicates the host lacks a required tool or supported capability.
	CodeHostIncompatible Code = "HOST_INCOMPATIBLE"
	// CodeDigestMismatch indicates content does not match its expected digest or diffID.
	CodeDigestMismatch Code = "IMAGE_DIGEST_MISMATCH"
	// CodeArtifactCorrupt indicates an artifact or metadata record has an invalid representation.
	CodeArtifactCorrupt Code = "ARTIFACT_CORRUPT"
	// CodeArtifactUnavailable indicates an artifact cannot be accessed or durably written.
	CodeArtifactUnavailable Code = "ARTIFACT_UNAVAILABLE"
	// CodeReferenced indicates an entity cannot be removed while live references remain.
	CodeReferenced Code = "REFERENCED"
	// CodeStoreBusy indicates the metadata engine cannot acquire a transaction within its budget.
	CodeStoreBusy Code = "STORE_BUSY"
	// CodeInternal indicates a failure has no more specific public classification.
	CodeInternal Code = "INTERNAL"
)

// Class groups codes that share handling policy.
type Class uint8

const (
	// ClassUnknown indicates the zero value has no handling classification.
	ClassUnknown Class = iota
	// ClassNotFound indicates the requested entity is absent.
	ClassNotFound
	// ClassInvalid indicates the caller must correct arguments or host requirements.
	ClassInvalid
	// ClassConflict indicates existing state prevents the requested change.
	ClassConflict
	// ClassUnavailable indicates a required resource is temporarily or operationally inaccessible.
	ClassUnavailable
	// ClassCorrupt indicates stored or supplied content violates integrity expectations.
	ClassCorrupt
	// ClassInternal indicates an unexpected implementation failure occurred.
	ClassInternal
)

// Error carries stable classification and diagnostic context across layers.
type Error struct {
	// Class selects broad handling policy independently of the diagnostic message.
	Class Class
	// Code identifies the failure for automation without parsing text.
	Code Code
	// Operation identifies the user-visible operation that failed.
	Operation string
	// Entity identifies the affected image, name, or other module record.
	Entity string
	// Phase locates failure within the operation lifecycle.
	Phase string
	// Committed records that durable business state changed despite this error;
	// callers must inspect resulting state before deciding to retry.
	Committed bool
	// Retry is an optional producer hint that another attempt may succeed.
	Retry bool
	// Action suggests a recovery step for the caller.
	Action string
	// Cause preserves underlying failures for errors.Is and errors.As.
	Cause error
}

// Error renders classification and available context, including the recovery action.
// A nil receiver is printable.
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

// Unwrap exposes the cause to standard error-chain inspection, including nil receivers.
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

// Context wraps err with operation context without mutating an existing Error.
// Nonempty supplied fields override prior context, and Committed can only become
// true. Unclassified errors receive ClassInternal/CodeInternal; nil remains nil.
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

// first keeps existing diagnostic context when the wrapping boundary omits a field.
func first(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
