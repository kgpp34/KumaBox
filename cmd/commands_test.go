package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func execute(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestDoctorFailsUntilTheRootExists(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "kb")
	code, _, stderr := execute(t, "doctor", "--root", root)
	if code != 6 {
		t.Errorf("exit code = %d, want 6\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "HOST_NOT_READY") {
		t.Errorf("stderr must carry the stable code:\n%s", stderr)
	}
}

func TestDoctorFixThenJSON(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "kb")
	if code, _, stderr := execute(t, "doctor", "--fix", "--root", root); code != 0 {
		t.Fatalf("doctor --fix exit code = %d, want 0\nstderr: %s", code, stderr)
	}

	code, stdout, stderr := execute(t, "doctor", "--json", "--root", root)
	if code != 0 {
		t.Fatalf("doctor --json exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("--json did not produce valid JSON: %v\n%s", err, stdout)
	}
	if report["phase"] != "S1" {
		t.Errorf("phase = %v, want S1", report["phase"])
	}
}

func TestInvalidRootIsAnInvalidArgument(t *testing.T) {
	t.Parallel()

	code, _, stderr := execute(t, "doctor", "--root", "relative/path")
	if code != 5 {
		t.Errorf("exit code = %d, want 5\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "ROOT_NOT_ABSOLUTE") {
		t.Errorf("stderr must carry the stable code:\n%s", stderr)
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	t.Parallel()

	code, _, stderr := execute(t, "frobnicate")
	if code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}
	if !strings.Contains(stderr, "code: USAGE") {
		t.Errorf("stderr must classify the failure as usage:\n%s", stderr)
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	t.Parallel()

	code, _, stderr := execute(t, "doctor", "--nope")
	if code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}
	if !strings.Contains(stderr, "code: USAGE") {
		t.Errorf("stderr must classify the failure as usage:\n%s", stderr)
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := execute(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", code, stderr)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("version --json is not valid JSON: %v\n%s", err, stdout)
	}
	if payload["version"] == "" {
		t.Error("version payload is empty")
	}
}
