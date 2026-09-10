package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/host"
	"github.com/kumabox/kumabox/layout"
)

// Exit codes are documented in docs/BEHAVIOR.md §17.
const (
	exitOK       = 0
	exitInternal = 1
	exitInvalid  = 5
	exitUnavail  = 6
)

// ErrUsage marks a failure of the command line itself: an unknown command, an
// unknown flag, or a bad argument.
var ErrUsage = errors.New("usage")

// fail prints a failure the way the CLI contract requires and returns the code
// the process should exit with.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "kumabox: %v\n", err)
	code, exit := classify(err)
	fmt.Fprintf(stderr, "code: %s\n", code)
	return exit
}

// classify maps a library error to its stable code and exit code.
//
// Library code returns sentinel errors wrapped with context; the mapping lives
// here, at the edge, so no library has to know about exit codes. Anything that
// reaches here without a known sentinel came from the command line itself,
// because every handler wraps its failures.
func classify(err error) (code string, exit int) {
	switch {
	case errors.Is(err, ErrUsage):
		return "USAGE", exitInvalid
	case errors.Is(err, config.ErrRootNotAbsolute), errors.Is(err, layout.ErrRootNotAbsolute):
		return "ROOT_NOT_ABSOLUTE", exitInvalid
	case errors.Is(err, layout.ErrRootEmpty):
		return "ROOT_EMPTY", exitInvalid
	case errors.Is(err, layout.ErrRootNotDirectory):
		return "ROOT_NOT_DIRECTORY", exitInvalid
	case errors.Is(err, layout.ErrRootNotWritable):
		return "ROOT_NOT_WRITABLE", exitUnavail
	case errors.Is(err, layout.ErrStatFailed):
		return "ROOT_STAT_FAILED", exitInternal
	case errors.Is(err, host.ErrNotReady):
		return "HOST_NOT_READY", exitUnavail
	default:
		return "USAGE", exitInvalid
	}
}

// writeJSON renders a command result as JSON on out.
func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
