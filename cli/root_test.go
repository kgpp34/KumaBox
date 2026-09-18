package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/errdefs"
)

func TestDoctorForwardsArgumentsAndExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}

	dir := t.TempDir()
	checker := filepath.Join(dir, "kumabox-check")
	contents := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\nexit 1\n")
	if err := os.WriteFile(checker, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout bytes.Buffer
	err := Execute(context.Background(), []string{
		"doctor", "--fix", "--upgrade", "--subnet=10.89.0.0/16",
	}, &stdout, &bytes.Buffer{})
	if got := ExitCode(err); got != 1 {
		t.Fatalf("ExitCode() = %d, want 1; err = %v", got, err)
	}
	if !Silent(err) {
		t.Fatal("doctor process error must be silent")
	}
	if got, want := stdout.String(), "--fix\n--upgrade\n--subnet=10.89.0.0/16\n"; got != want {
		t.Fatalf("forwarded arguments = %q, want %q", got, want)
	}
}

func TestVersion(t *testing.T) {
	var stdout bytes.Buffer
	err := Execute(context.Background(), []string{"version"}, &stdout, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "kumabox ") {
		t.Fatalf("version output = %q", stdout.String())
	}
}

func TestExecuteConfigurationIsInvocationLocal(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"first", "second"} {
		dataRoot := filepath.Join(base, name, "data")
		configFile := filepath.Join(base, name+".yaml")
		contents := []byte("paths:\n  data: " + dataRoot + "\n  run: " + filepath.Join(base, name, "run") + "\n  log: " + filepath.Join(base, name, "log") + "\n")
		if err := os.WriteFile(configFile, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout bytes.Buffer
		if err := Execute(t.Context(), []string{"--config", configFile, "image", "ls", "--json"}, &stdout, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if stdout.String() != "[]\n" {
			t.Fatalf("%s output = %q", name, stdout.String())
		}
		if _, err := os.Stat(filepath.Join(dataRoot, "meta", "meta.db")); err != nil {
			t.Fatalf("%s invocation did not use its config: %v", name, err)
		}
	}
}

func TestExecutePassesConfigurationToImageAdapters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test converter is a POSIX shell script")
	}
	base := t.TempDir()
	binary := filepath.Join(base, "configured-erofs")
	marker := filepath.Join(base, "converter-used")
	t.Setenv("TEST_EROFS_MARKER", marker)
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then : > \"$TEST_EROFS_MARKER\"; printf 'mkfs.erofs 1.8.10\\n'; exit 0; fi\nfor output do :; done\n/bin/cat > \"$output\"\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(base, "config.yaml")
	contents := []byte("paths:\n  data: " + filepath.Join(base, "data") + "\n  run: " + filepath.Join(base, "run") + "\n  log: " + filepath.Join(base, "log") + "\nimages:\n  erofs_binary: " + binary + "\n  parallelism: 1\n")
	if err := os.WriteFile(configFile, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(t.Context(), []string{
		"--config", configFile, "image", "import", "configured", "../testdata/oci-layout", "--platform", "linux/amd64",
	}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("configured converter was not invoked: %v", err)
	}
}

func TestInvalidConfigurationUsesDomainExitCode(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(configFile, []byte("images:\n  parallelism: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Execute(t.Context(), []string{"--config", configFile, "version"}, &bytes.Buffer{}, &bytes.Buffer{})
	if got := ExitCode(err); got != 5 {
		t.Fatalf("exit = %d, want 5; error = %v", got, err)
	}
}

func TestImageAndUsageExitCodes(t *testing.T) {
	base := t.TempDir()
	flags := []string{"--root-dir", filepath.Join(base, "data"), "--run-dir", filepath.Join(base, "run"), "--log-dir", filepath.Join(base, "log")}
	for _, test := range []struct {
		name string
		args []string
		code int
	}{
		{"unknown command", []string{"unknown"}, 2},
		{"unknown image command", []string{"image", "unknown"}, 2},
		{"missing image argument", []string{"image", "inspect"}, 2},
		{"missing create image", []string{"create", "--name", "box"}, 2},
		{"missing console sandbox", []string{"console"}, 2},
		{"missing exec command", []string{"exec", "box"}, 2},
		{"missing inspect sandbox", []string{"inspect"}, 2},
		{"missing remove sandbox", []string{"rm"}, 2},
		{"missing start sandbox", []string{"start"}, 2},
		{"missing stop sandbox", []string{"stop"}, 2},
		{"unexpected ps argument", []string{"ps", "box"}, 2},
		{"unsupported inspect flag", []string{"inspect", "box", "--json"}, 2},
		{"unknown flag", []string{"image", "ls", "--wrong"}, 2},
		{"unsupported platform", []string{"image", "pull", "example.com/image", "--platform", "windows/amd64"}, 5},
		{"incompatible ps output", []string{"ps", "--json", "--quiet"}, 5},
		{"missing image", []string{"image", "inspect", "missing"}, 3},
		{"missing inspected sandbox", []string{"inspect", "missing"}, 3},
		{"missing sandbox", []string{"rm", "missing"}, 3},
		{"empty list", []string{"image", "ls", "--json"}, 0},
		{"empty ps", []string{"ps", "--all", "--json"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append(append([]string(nil), flags...), test.args...)
			err := Execute(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{})
			if got := ExitCode(err); got != test.code {
				t.Fatalf("exit = %d, want %d, error %v", got, test.code, err)
			}
		})
	}
}

func TestDomainErrorExitCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		code errdefs.Code
		want int
	}{
		{"not found", errdefs.CodeNotFound, 3},
		{"name taken", errdefs.CodeNameTaken, 4},
		{"state conflict", errdefs.CodeStateConflict, 4},
		{"referenced", errdefs.CodeReferenced, 4},
		{"invalid argument", errdefs.CodeInvalidArgument, 5},
		{"host incompatible", errdefs.CodeHostIncompatible, 5},
		{"image incompatible", errdefs.CodeImageIncompatible, 5},
		{"digest mismatch", errdefs.CodeDigestMismatch, 5},
		{"artifact corrupt", errdefs.CodeArtifactCorrupt, 5},
		{"artifact unavailable", errdefs.CodeArtifactUnavailable, 6},
		{"store busy", errdefs.CodeStoreBusy, 6},
		{"internal", errdefs.CodeInternal, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := errdefs.Context(
				errdefs.New(errdefs.ClassInternal, test.code, errors.New("failure")),
				"operation", "entity", "phase", "action", false,
			)
			if got := errorExitCode(err); got != test.want {
				t.Fatalf("errorExitCode(%q) = %d, want %d", test.code, got, test.want)
			}
		})
	}
}
