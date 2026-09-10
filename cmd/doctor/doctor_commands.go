// Package doctor exposes the Cocoon-compatible host checker through kumabox.
package doctor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

const checkerName = "kumabox-check"

type processError struct {
	err  error
	code int
}

func (e *processError) Error() string { return e.err.Error() }
func (e *processError) Unwrap() error { return e.err }
func (e *processError) ExitCode() int { return e.code }
func (e *processError) Silent() bool  { return true }

// NewCommand returns the doctor command. Flag parsing belongs to check.sh, so
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

			check := exec.CommandContext(command.Context(), path, args...)
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
