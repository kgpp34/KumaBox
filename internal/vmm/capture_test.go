package vmm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/types"
)

func TestCaptureIntentSurvivesReopenAndClearsIdempotently(t *testing.T) {
	base := t.TempDir()
	roots := storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	paths, err := NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	process := Process{PID: 42, StartTicks: 7, BootID: "boot", SandboxID: testSandboxID, Generation: 3, Binary: "cloud-hypervisor", APISocket: "/run/api.sock"}
	for _, action := range []CaptureAction{CaptureResume, CaptureStop} {
		intent := CaptureIntent{Process: process, Destination: "/capture", Action: action}
		if err := paths.WriteCapture(intent); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewPaths(roots)
		if err != nil {
			t.Fatal(err)
		}
		got, err := reopened.ReadCapture(testSandboxID)
		if err != nil || got != intent {
			t.Fatalf("intent = %+v, %v", got, err)
		}
	}
	for range 2 {
		if err := paths.ClearCapture(testSandboxID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := paths.ReadCapture(testSandboxID); !os.IsNotExist(err) {
		t.Fatalf("cleared intent = %v", err)
	}
	if err := paths.WriteCapture(CaptureIntent{}); err == nil {
		t.Fatal("invalid identity accepted")
	}
}

func TestCaptureIntentRejectsUnknownActionAndCorruptIdentity(t *testing.T) {
	base := t.TempDir()
	paths, err := NewPaths(storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")})
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Prepare(testSandboxID); err != nil {
		t.Fatal(err)
	}
	process := Process{PID: 42, StartTicks: 7, BootID: "boot", SandboxID: testSandboxID, Generation: 3, Binary: "cloud-hypervisor", APISocket: "/run/api.sock"}
	for _, test := range []struct {
		name   string
		action CaptureAction
		id     string
	}{
		{name: "missing action", id: testSandboxID.String()},
		{name: "unknown action", action: "future-operation", id: testSandboxID.String()},
		{name: "foreign identity", action: CaptureResume, id: "223e4567-e89b-42d3-a456-426614174000"},
		{name: "invalid identity", action: CaptureResume, id: "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := CaptureIntent{Process: process, Destination: "/capture", Action: test.action}
			intent.Process.SandboxID = types.SandboxID(test.id)
			data, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			name, err := paths.runFile(testSandboxID, "capture.json")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := paths.ReadCapture(testSandboxID); err == nil {
				t.Fatal("invalid recovery intent accepted")
			}
		})
	}
}
