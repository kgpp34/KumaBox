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

	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/storage"
)

func TestDoctorForwardsArgumentsAndExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}

	dir := shortTempDir(t)
	checker := filepath.Join(dir, "kumabox-check")
	contents := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\nexit 1\n")
	if err := os.WriteFile(checker, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout bytes.Buffer
	cfg := config.Default()
	cfg.Paths = storage.Roots{Data: filepath.Join(dir, "data"), Run: filepath.Join(dir, "run"), Log: filepath.Join(dir, "log")}
	startTestNode(t, cfg)
	err := Execute(context.Background(), []string{
		"--socket", cfg.SocketPath(),
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
	base := shortTempDir(t)
	for _, name := range []string{"first", "second"} {
		dataRoot := filepath.Join(base, name, "data")
		configFile := filepath.Join(base, name+".yaml")
		contents := []byte("paths:\n  data: " + dataRoot + "\n  run: " + filepath.Join(base, name, "run") + "\n  log: " + filepath.Join(base, name, "log") + "\n")
		if err := os.WriteFile(configFile, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.NewLoader().Load(configFile)
		if err != nil {
			t.Fatal(err)
		}
		startTestNode(t, cfg)
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
	base := shortTempDir(t)
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
	cfg, err := config.NewLoader().Load(configFile)
	if err != nil {
		t.Fatal(err)
	}
	startTestNode(t, cfg)
	if err := Execute(t.Context(), []string{
		"--config", configFile, "image", "import", "configured", "../../testdata/oci-layout", "--platform", "linux/amd64",
	}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("configured converter was not invoked: %v", err)
	}
}

func TestInvalidConfigurationUsesDomainExitCode(t *testing.T) {
	configFile := filepath.Join(shortTempDir(t), "invalid.yaml")
	if err := os.WriteFile(configFile, []byte("images:\n  parallelism: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Execute(t.Context(), []string{"--config", configFile, "version"}, &bytes.Buffer{}, &bytes.Buffer{})
	if got := ExitCode(err); got != 5 {
		t.Fatalf("exit = %d, want 5; error = %v", got, err)
	}
}

func TestNetworkFlagsAreBoundIntoConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{"--cni-conf-dir", "relative", "version"},
		{"--cni-bin-dir", "relative", "version"},
		{"--dns", "not-an-ip", "version"},
	} {
		err := Execute(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{})
		if got := ExitCode(err); got != 5 {
			t.Fatalf("Execute(%v) exit = %d, want 5; error = %v", args, got, err)
		}
	}
}

func TestImageAndUsageExitCodes(t *testing.T) {
	base := shortTempDir(t)
	cfg := config.Default()
	cfg.Paths = storage.Roots{Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log")}
	startTestNode(t, cfg)
	flags := []string{"--run-dir", filepath.Join(base, "run")}
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
		{"missing forward ports", []string{"forward", "box"}, 2},
		{"invalid forward guest port", []string{"forward", "box", "8080:0"}, 5},
		{"public forward listener", []string{"forward", "box", "8080:80", "--address", "0.0.0.0"}, 5},
		{"missing inspect sandbox", []string{"inspect"}, 2},
		{"missing logs sandbox", []string{"logs"}, 2},
		{"missing fs attach sandbox", []string{"fs", "attach"}, 2},
		{"missing hibernate sandbox", []string{"hibernate"}, 2},
		{"missing remove sandbox", []string{"rm"}, 2},
		{"missing restore references", []string{"restore", "box"}, 2},
		{"missing run image", []string{"run", "--name", "box"}, 2},
		{"missing start sandbox", []string{"start"}, 2},
		{"missing stop sandbox", []string{"stop"}, 2},
		{"missing snapshot save sandbox", []string{"snapshot", "save"}, 2},
		{"missing snapshot inspect reference", []string{"snapshot", "inspect"}, 2},
		{"missing snapshot remove reference", []string{"snapshot", "rm"}, 2},
		{"unexpected snapshot list argument", []string{"snapshot", "ls", "extra"}, 2},
		{"unexpected ps argument", []string{"ps", "box"}, 2},
		{"unsupported inspect flag", []string{"inspect", "box", "--json"}, 2},
		{"unknown flag", []string{"image", "ls", "--wrong"}, 2},
		{"unsupported platform", []string{"image", "pull", "example.com/image", "--platform", "windows/amd64"}, 5},
		{"incompatible ps output", []string{"ps", "--json", "--quiet"}, 5},
		{"missing image", []string{"image", "inspect", "missing"}, 3},
		{"missing inspected sandbox", []string{"inspect", "missing"}, 3},
		{"missing logged sandbox", []string{"logs", "missing"}, 3},
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
			err := errdefs.WithContext(errdefs.New(errdefs.ClassInternal, test.code, errors.New("failure")), errdefs.ContextInfo{
				Operation: "operation",
				Entity:    "entity",
				Phase:     "phase",
				Action:    "action",
			})
			if got := errorExitCode(err); got != test.want {
				t.Fatalf("errorExitCode(%q) = %d, want %d", test.code, got, test.want)
			}
		})
	}
}

func TestMissingDaemonNeverCreatesRuntimeState(t *testing.T) {
	base := shortTempDir(t)
	socket := filepath.Join(base, "missing.sock")
	err := Execute(t.Context(), []string{"--socket", socket, "image", "ls", "--json"}, &bytes.Buffer{}, &bytes.Buffer{})
	if ExitCode(err) != 6 || !strings.Contains(err.Error(), "start kumabox daemon") {
		t.Fatalf("missing daemon: %v", err)
	}
	entries, readErr := os.ReadDir(base)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("client opened runtime state: %v %v", entries, readErr)
	}
}

func TestHostPolicyFlagsAreDaemonOnly(t *testing.T) {
	err := Execute(t.Context(), []string{"--root-dir", filepath.Join(shortTempDir(t), "data"), "ps"}, &bytes.Buffer{}, &bytes.Buffer{})
	if ExitCode(err) != 5 || !strings.Contains(err.Error(), "configures the daemon") {
		t.Fatalf("host flag on client: %v", err)
	}
}
