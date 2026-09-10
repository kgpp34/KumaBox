package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/host"
)

func TestDoctorReportsAMissingRoot(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "kb")
	report, err := Handler{}.Doctor(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !report.Failed() {
		t.Fatal("a missing root must fail the phase being checked")
	}
	// The root is canonicalised, so compare against the resolved path rather
	// than the platform's spelling of the temp directory.
	resolved, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if want := filepath.Join(resolved, "kb"); report.Root != want {
		t.Errorf("report root = %q, want %q", report.Root, want)
	}
	if state := stateOf(t, report, "root-directory"); state != host.StateMissing {
		t.Errorf("root-directory state = %s, want missing", state)
	}
	if err := (Handler{}).NotReady(report); err == nil {
		t.Error("NotReady must report an error for a failed report")
	}
}

func TestDoctorFixCreatesTheRootAndThenPasses(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "kb")
	handler := Handler{}

	if _, err := handler.Doctor(context.Background(), Options{Root: root, Fix: true}); err != nil {
		t.Fatalf("Doctor --fix: %v", err)
	}
	for _, dir := range []string{"metadata", "blobs", "tmp"} {
		if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
			t.Errorf("--fix did not create %s: %v", dir, err)
		}
	}

	report, err := handler.Doctor(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.Failed() {
		t.Errorf("a repaired root must pass phase %s: %+v", report.Phase, report.Checks)
	}
	if err := handler.NotReady(report); err != nil {
		t.Errorf("NotReady = %v, want nil", err)
	}
}

func TestDoctorRejectsARelativeRoot(t *testing.T) {
	t.Parallel()

	if _, err := (Handler{}).Doctor(context.Background(), Options{Root: "relative/path"}); err == nil {
		t.Fatal("Doctor must reject a relative root")
	}
}

func TestRenderJSONIsStableAndReadable(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "kb")
	report, err := Handler{}.Doctor(context.Background(), Options{Root: root, Fix: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}

	var buffer bytes.Buffer
	if err := (Handler{}).Render(&buffer, report, true); err != nil {
		t.Fatalf("Render: %v", err)
	}

	var decoded struct {
		Phase  string `json:"phase"`
		OS     string `json:"os"`
		Root   string `json:"root"`
		Checks []struct {
			Name  string `json:"name"`
			State string `json:"state"`
			Since string `json:"since"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(buffer.Bytes(), &decoded); err != nil {
		t.Fatalf("Render --json produced invalid JSON: %v\n%s", err, buffer.String())
	}
	if decoded.Phase != "S1" {
		t.Errorf("phase = %q, want S1 as text", decoded.Phase)
	}
	if decoded.Root != report.Root {
		t.Errorf("root = %q, want %q", decoded.Root, report.Root)
	}
	if len(decoded.Checks) == 0 {
		t.Fatal("the report must contain checks")
	}
	if decoded.Checks[0].State == "" || decoded.Checks[0].Since == "" {
		t.Errorf("check states and phases must be text, got %+v", decoded.Checks[0])
	}
}

func TestRenderTextListsFixes(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "kb")
	report, err := Handler{}.Doctor(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}

	var buffer bytes.Buffer
	if err := (Handler{}).Render(&buffer, report, false); err != nil {
		t.Fatalf("Render: %v", err)
	}
	text := buffer.String()
	for _, want := range []string{"CHECK", "root-directory", "fix:", "--fix"} {
		if !strings.Contains(text, want) {
			t.Errorf("text report does not mention %q:\n%s", want, text)
		}
	}
}

func stateOf(t *testing.T, report host.Report, name string) host.State {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check.State
		}
	}
	t.Fatalf("report has no check %q", name)
	return host.StateOK
}
