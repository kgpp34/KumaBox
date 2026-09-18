package errdefs

import (
	"errors"
	"strings"
	"testing"
)

func TestContextClassifiesUnclassifiedError(t *testing.T) {
	cause := errors.New("read metadata")
	err := Context(cause, "inspect sandbox", "box", "metadata", "retry the query", false)

	var classified *Error
	if !errors.As(err, &classified) {
		t.Fatal("Context() did not return a classified error")
	}
	if classified.Class != ClassInternal || classified.Code != CodeInternal {
		t.Fatalf("classification = (%d, %q), want (%d, %q)", classified.Class, classified.Code, ClassInternal, CodeInternal)
	}
	if !errors.Is(err, cause) {
		t.Fatal("Context() did not preserve the original cause")
	}
	if got, want := err.Error(), "inspect sandbox: INTERNAL (box) at metadata: read metadata; retry the query"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestContextReplacesPresentationWithoutDuplicatingClassification(t *testing.T) {
	cause := errors.New("connect guest vsock")
	original := New(ClassUnavailable, CodeArtifactUnavailable, cause)
	first := Context(original, "connect agent", "box", "dial", "retry shortly", false)
	second := Context(first, "execute sandbox command", "", "run", "inspect the guest agent", true)

	got := second.Error()
	if count := strings.Count(got, string(CodeArtifactUnavailable)); count != 1 {
		t.Fatalf("Error() contains the classification %d times, want once: %q", count, got)
	}
	if want := "execute sandbox command: ARTIFACT_UNAVAILABLE (box) at run: connect guest vsock; inspect the guest agent"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}

	var classified *Error
	if !errors.As(second, &classified) {
		t.Fatal("errors.As() did not find the outer classified error")
	}
	if classified.Operation != "execute sandbox command" || classified.Entity != "box" || classified.Phase != "run" || !classified.Committed {
		t.Fatalf("outer context = %#v", classified)
	}
	if !errors.Is(second, cause) || !errors.Is(second, original) {
		t.Fatal("Context() did not preserve the original error chain")
	}
	if original.Operation != "" || original.Entity != "" || original.Phase != "" || original.Committed {
		t.Fatalf("Context() mutated the original error: %#v", original)
	}
	if code, ok := CodeOf(second); !ok || code != CodeArtifactUnavailable {
		t.Fatalf("CodeOf() = (%q, %t), want (%q, true)", code, ok, CodeArtifactUnavailable)
	}
}

func TestContextPreservesJoinedErrors(t *testing.T) {
	cause := errors.New("write metadata")
	cleanup := errors.New("close metadata")
	original := New(ClassUnavailable, CodeArtifactUnavailable, cause)
	err := Context(errors.Join(original, cleanup), "create sandbox", "box", "commit", "inspect the sandbox", true)

	got := err.Error()
	for _, message := range []string{string(CodeArtifactUnavailable), cause.Error(), cleanup.Error()} {
		if count := strings.Count(got, message); count != 1 {
			t.Fatalf("Error() contains %q %d times, want once: %q", message, count, got)
		}
	}
	if !errors.Is(err, original) || !errors.Is(err, cause) || !errors.Is(err, cleanup) {
		t.Fatal("Context() did not preserve every branch of the joined error")
	}

	var classified *Error
	if !errors.As(err, &classified) || !classified.Committed {
		t.Fatalf("errors.As() = %#v, want committed classified error", classified)
	}
}

func TestContextNil(t *testing.T) {
	if err := Context(nil, "operation", "entity", "phase", "action", true); err != nil {
		t.Fatalf("Context(nil) = %v, want nil", err)
	}
}
