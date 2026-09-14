// Package cli builds the kumabox command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	doctorcmd "github.com/kumabox/kumabox/cli/doctor"
	imagecmd "github.com/kumabox/kumabox/cli/image"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/version"
)

type exitCoder interface {
	ExitCode() int
}

type silentError interface {
	Silent() bool
}

type codedError struct {
	err  error
	code int
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }
func (e *codedError) ExitCode() int { return e.code }

const exitUsage = 2

// Execute runs one CLI invocation.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root := newRootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	// Resolve unknown commands before execution so Cobra usage errors keep exit 2.
	if _, _, err := root.Find(args); err != nil {
		return &codedError{err: err, code: exitUsage}
	}
	err := root.ExecuteContext(ctx)
	if err == nil {
		return nil
	}
	var coded exitCoder
	if errors.As(err, &coded) {
		return err
	}
	return &codedError{err: err, code: errorExitCode(err)}
}

// ExitCode returns the process exit status represented by err.
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

// Silent reports whether the command already wrote its diagnostic output.
func Silent(err error) bool {
	var silent silentError
	return errors.As(err, &silent) && silent.Silent()
}

func newRootCommand() *cobra.Command {
	roots := storage.DefaultRoots()
	root := &cobra.Command{
		Use:           "kumabox",
		Args:          cobra.NoArgs,
		RunE:          func(command *cobra.Command, _ []string) error { return command.Help() },
		Short:         "microVM sandboxes for AI agents",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &codedError{err: err, code: exitUsage}
	})
	root.PersistentFlags().StringVar(&roots.Data, "root-dir", roots.Data, "persistent data directory")
	root.PersistentFlags().StringVar(&roots.Run, "run-dir", roots.Run, "runtime state directory")
	root.PersistentFlags().StringVar(&roots.Log, "log-dir", roots.Log, "log directory")

	root.AddCommand(doctorcmd.NewCommand())
	root.AddCommand(imagecmd.NewCommand(func() storage.Roots { return roots }))
	root.AddCommand(newVersionCommand())
	classifyArguments(root)
	return root
}

func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(command *cobra.Command, args []string) error {
		if err := validate(command, args); err != nil {
			return &codedError{err: err, code: exitUsage}
		}
		return nil
	}
}

func classifyArguments(command *cobra.Command) {
	if command.Args != nil {
		command.Args = usageArgs(command.Args)
	}
	for _, child := range command.Commands() {
		classifyArguments(child)
	}
}

func errorExitCode(err error) int {
	code, ok := errdefs.CodeOf(err)
	if !ok {
		return 1
	}
	switch code {
	case errdefs.CodeNotFound:
		return 3
	case errdefs.CodeNameTaken, errdefs.CodeReferenced:
		return 4
	case errdefs.CodeInvalidArgument, errdefs.CodeHostIncompatible, errdefs.CodeDigestMismatch, errdefs.CodeArtifactCorrupt:
		return 5
	case errdefs.CodeArtifactUnavailable, errdefs.CodeStoreBusy:
		return 6
	default:
		return 1
	}
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
