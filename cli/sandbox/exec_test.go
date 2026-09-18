package sandbox

import (
	"strings"
	"testing"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
)

func TestParseEnvironment(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    map[string]string
		wantErr string
	}{
		{name: "empty", entries: nil, want: nil},
		{name: "values", entries: []string{"A=one", "EMPTY="}, want: map[string]string{"A": "one", "EMPTY": ""}},
		{name: "value contains equals", entries: []string{"A=one=two"}, want: map[string]string{"A": "one=two"}},
		{name: "duplicate uses last value", entries: []string{"A=one", "A=two"}, want: map[string]string{"A": "two"}},
		{name: "missing separator", entries: []string{"A"}, wantErr: "must be KEY=VALUE"},
		{name: "empty key", entries: []string{"=value"}, wantErr: "must be KEY=VALUE"},
		{name: "NUL", entries: []string{"A=bad\x00value"}, wantErr: "must not contain NUL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseEnvironment(test.entries)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseEnvironment(%q) error = %v, want %q", test.entries, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("environment = %#v, want %#v", got, test.want)
			}
			for key, value := range test.want {
				if got[key] != value {
					t.Fatalf("environment[%q] = %q, want %q", key, got[key], value)
				}
			}
		})
	}
}

func TestCommandExitErrorPreservesGuestStatus(t *testing.T) {
	err := &commandExitError{code: 23}
	if err.ExitCode() != 23 || !err.Silent() || !strings.Contains(err.Error(), "23") {
		t.Fatalf("command exit error = %#v, %q", err, err.Error())
	}
}

func TestExecCommandRejectsEnvironmentBeforeOpeningService(t *testing.T) {
	command := NewExecCommand(func() config.Config {
		t.Fatal("environment validation opened the sandbox service")
		return config.Config{}
	})
	command.SetArgs([]string{"box", "--env", "BROKEN", "--", "env"})
	err := command.ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "--env") {
		t.Fatalf("exec error = %v, want --env context", err)
	}
	if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeInvalidArgument {
		t.Fatalf("exec error code = %q, %v", code, ok)
	}
}
