// Package cmd builds the kumabox command tree.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	doctorcmd "github.com/kumabox/kumabox/cmd/doctor"
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
	root := &cobra.Command{
		Use:           "kumabox",
		Short:         "microVM sandboxes for AI agents",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &codedError{err: err, code: exitUsage}
	})

	root.AddCommand(doctorcmd.NewCommand())
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
