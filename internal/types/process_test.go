package types

import (
	"strings"
	"testing"
)

func TestValidateProcessIDAndSignal(t *testing.T) {
	for _, test := range []struct {
		name, id string
		valid    bool
	}{
		{name: "handle", id: strings.Repeat("a", 32), valid: true},
		{name: "raw PID", id: "123"},
		{name: "invalid hex", id: strings.Repeat("z", 32)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateProcessID(test.id); (err == nil) != test.valid {
				t.Fatalf("ID validation = %v", err)
			}
		})
	}
	for _, signal := range []ProcessSignal{ProcessSignalTERM, ProcessSignalKILL, ProcessSignalHUP, ProcessSignalINT} {
		t.Run(string(signal), func(t *testing.T) {
			if err := signal.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := ProcessSignal("STOP").Validate(); err == nil {
		t.Fatal("unsupported signal accepted")
	}
}

func TestCommandValidatesWorkingDirectoryAndUser(t *testing.T) {
	for _, test := range []struct {
		name, cwd, user string
		valid           bool
	}{
		{name: "inherit", valid: true},
		{name: "directory with spaces", cwd: "/work/my project", valid: true},
		{name: "relative directory", cwd: "work"},
		{name: "directory NUL", cwd: "/work\x00"},
		{name: "username", user: "worker", valid: true},
		{name: "UID", user: "1000", valid: true},
		{name: "unsupported group syntax", user: "worker:root"},
		{name: "user NUL", user: "worker\x00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (Command{Args: []string{"true"}, Cwd: test.cwd, User: test.user}).Validate()
			if (err == nil) != test.valid {
				t.Fatalf("command validation = %v", err)
			}
		})
	}
}
