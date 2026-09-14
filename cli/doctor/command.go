// Package doctor exposes the Cocoon-compatible host checker through kumabox.
package doctor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// checkerName resolves the separately installed host-check script through PATH.
const checkerName = "kumabox-check"

// processError preserves the checker's exit status without printing its diagnostics twice.
type processError struct {
	// err retains the subprocess failure for errors.As and errors.Is.
	err error
	// code is the checker process exit status.
	code int
}

// Error forwards the original subprocess failure message.
func (e *processError) Error() string { return e.err.Error() }

// Unwrap preserves access to the original exec.ExitError.
func (e *processError) Unwrap() error { return e.err }

// ExitCode propagates the checker's status to the kumabox process.
func (e *processError) ExitCode() int { return e.code }

// Silent reports that the checker already wrote its own diagnostics.
func (e *processError) Silent() bool { return true }

// NewCommand returns the doctor command. Flag parsing belongs to kumabox-check, so
// every argument after "doctor" is forwarded unchanged.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "doctor [--fix] [--upgrade] [--subnet=CIDR]",
		Short:              "check and repair host prerequisites",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(command *cobra.Command, args []string) error {
			path, err := exec.LookPath(checkerName)
			if err != nil {
				return fmt.Errorf("find %s: %w", checkerName, err)
			}

			check := exec.CommandContext(command.Context(), path, args...) //nolint:gosec // executable is resolved by name from the operator-controlled PATH
			check.Stdin = command.InOrStdin()
			check.Stdout = command.OutOrStdout()
			check.Stderr = command.ErrOrStderr()
			if err := check.Run(); err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					return &processError{err: err, code: exitErr.ExitCode()}
				}
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("run %s: %w", checkerName, err)
				}
				return err
			}
			return nil
		},
	}
}
