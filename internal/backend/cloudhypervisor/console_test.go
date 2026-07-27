package cloudhypervisor

import "testing"

func TestIsPTYConsoleMode(t *testing.T) {
	tests := map[string]bool{
		"pty":   true,
		"Pty":   true,
		"PTY":   true,
		" pty ": true,
		"file":  false,
		"":      false,
	}
	for mode, want := range tests {
		t.Run(mode, func(t *testing.T) {
			if got := isPTYConsoleMode(mode); got != want {
				t.Fatalf("isPTYConsoleMode(%q) = %t, want %t", mode, got, want)
			}
		})
	}
}
