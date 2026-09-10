// Package cmd builds the kumabox command tree.
//
// It owns flags, help and exit codes only. Each command lives in its own
// package and exposes a single NewCommand constructor; no command implements
// its logic here (docs/ARCHITECTURE.md §1).
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/version"
)

// exitCoder is implemented by errors that know which exit code they deserve.
// Commands classify their own failures this way, so no shared error package is
// needed and a library never has to know about exit codes.
type exitCoder interface{ ExitCode() int }

// codedError attaches an exit code to an error.
type codedError struct {
	err  error
	code int
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }
func (e *codedError) ExitCode() int { return e.code }

// exitUsage is the exit code for a failure of the command line itself.
const exitUsage = 5

// Execute runs one invocation and returns the error it failed with, or nil.
//
// Errors raised while resolving or parsing the command line are reported as
// usage failures; everything else is expected to carry its own exit code.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root := newRootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return nil
	}
	var coded exitCoder
	if errors.As(err, &coded) {
		return err
	}
	return &codedError{err: err, code: exitUsage}
}

// ExitCode maps the result of Execute to the process exit code documented in
// docs/BEHAVIOR.md §17.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var coded exitCoder
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return 1
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "kumabox",
		Short: "microVM sandboxes for AI agents",
		Long: "kumabox runs microVM sandboxes on this machine.\n\n" +
			"Every command opens the node root, does one job and exits; there is no\n" +
			"daemon in this version. Use --root to point at another root, for example\n" +
			"while developing.\n\n" +
			"Run doctor/check.sh first to see whether this machine is ready.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("root", "",
		"node root directory (default $KUMABOX_ROOT, then /var/lib/kumabox)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &codedError{err: err, code: exitUsage}
	})

	root.AddCommand(newVersionCommand())
	return root
}

func newVersionCommand() *cobra.Command {
	var asJSON bool

	command := &cobra.Command{
		Use:   "version",
		Short: "print the version",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if asJSON {
				return version.WriteJSON(command.OutOrStdout())
			}
			_, err := fmt.Fprintln(command.OutOrStdout(), version.String())
			return err
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the version as JSON")
	return command
}
