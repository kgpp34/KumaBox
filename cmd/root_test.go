package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func execute(t *testing.T, args ...string) (error, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	err := Execute(context.Background(), args, &stdout, &stderr)
	return err, stdout.String(), stderr.String()
}

func TestVersion(t *testing.T) {
	t.Parallel()

	err, stdout, stderr := execute(t, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v\nstderr: %s", err, stderr)
	}
	var payload map[string]string
	if jsonErr := json.Unmarshal([]byte(stdout), &payload); jsonErr != nil {
		t.Fatalf("version --json is not valid JSON: %v\n%s", jsonErr, stdout)
	}
	if payload["version"] == "" {
		t.Error("version payload is empty")
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	t.Parallel()

	err, _, _ := execute(t, "frobnicate")
	if err == nil {
		t.Fatal("an unknown command must fail")
	}
	if code := ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %v, want it to name the unknown command", err)
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	t.Parallel()

	err, _, _ := execute(t, "version", "--nope")
	if err == nil {
		t.Fatal("an unknown flag must fail")
	}
	if code := ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}
}

func TestSuccessExitsZero(t *testing.T) {
	t.Parallel()

	if code := ExitCode(nil); code != 0 {
		t.Errorf("ExitCode(nil) = %d, want 0", code)
	}
}
