// Package cli builds the kumabox command tree and maps command failures to exit statuses.
// Commands receive explicit streams and immutable configuration for independent invocations.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	doctorcmd "github.com/kumabox/kumabox/cli/doctor"
	imagecmd "github.com/kumabox/kumabox/cli/image"
	sandboxcmd "github.com/kumabox/kumabox/cli/sandbox"
	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/version"
)

// exitCoder preserves an explicit usage or subprocess status through error wrapping.
type exitCoder interface {
	// ExitCode is the process status to return for this failure.
	ExitCode() int
}

// silentError identifies failures whose diagnostics were already written by a command.
type silentError interface {
	// Silent suppresses the entry point's additional diagnostic when true.
	Silent() bool
}

// codedError attaches a CLI status while preserving the original error chain.
type codedError struct {
	// err is the original usage or command failure.
	err error
	// code is the status returned by the process entry point.
	code int
}

// Error retains the diagnostic produced by the underlying failure.
func (e *codedError) Error() string { return e.err.Error() }

// Unwrap keeps errors.Is and errors.As available to callers.
func (e *codedError) Unwrap() error { return e.err }

// ExitCode supplies the CLI status attached during command execution.
func (e *codedError) ExitCode() int { return e.code }

// exitUsage distinguishes malformed invocations from domain operation failures.
const exitUsage = 2

// Execute runs one CLI invocation using the supplied context and output streams.
// It returns errors without printing them; the process entry point prints diagnostics
// unless Silent reports that a command already handled them.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root, err := newRootCommand()
	if err != nil {
		return err
	}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	// Resolve unknown commands before execution so Cobra usage errors keep exit 2.
	if _, _, err := root.Find(args); err != nil {
		return &codedError{err: err, code: exitUsage}
	}
	err = root.ExecuteContext(ctx)
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

// newRootCommand creates invocation-local flags, configuration loading, and
// command modules. The provider observes the snapshot resolved after flag parsing.
func newRootCommand() (*cobra.Command, error) {
	loader := config.NewLoader()
	configuration := config.Default()
	configFile := ""
	root := &cobra.Command{
		Use:           "kumabox",
		Args:          cobra.NoArgs,
		RunE:          func(command *cobra.Command, _ []string) error { return command.Help() },
		Short:         "microVM sandboxes for AI agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			resolved, err := loader.Load(configFile)
			if err != nil {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
			}
			configuration = resolved
			return nil
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &codedError{err: err, code: exitUsage}
	})
	flags := root.PersistentFlags()
	flags.StringVar(&configFile, "config", "", "explicit configuration file")
	flags.String("root-dir", configuration.Paths.Data, "persistent data directory")
	flags.String("run-dir", configuration.Paths.Run, "runtime state directory")
	flags.String("log-dir", configuration.Paths.Log, "log directory")
	for key, name := range map[string]string{
		"paths.data": "root-dir", "paths.run": "run-dir", "paths.log": "log-dir",
	} {
		if err := loader.BindFlag(key, flags.Lookup(name)); err != nil {
			return nil, fmt.Errorf("bind --%s: %w", name, err)
		}
	}
	provideConfig := func() config.Config { return configuration }

	root.AddCommand(doctorcmd.NewCommand())
	root.AddCommand(imagecmd.NewCommand(provideConfig))
	root.AddCommand(sandboxcmd.NewConsoleCommand(provideConfig))
	root.AddCommand(sandboxcmd.NewCreateCommand(provideConfig))
	root.AddCommand(sandboxcmd.NewExecCommand(provideConfig))
	root.AddCommand(sandboxcmd.NewInspectCommand(provideConfig))
	root.AddCommand(sandboxcmd.NewListCommand(provideConfig))
	root.AddCommand(sandboxcmd.NewLogsCommand(provideConfig))
	root.AddCommand(sandboxcmd.NewRemoveCommand(provideConfig))
	root.AddCommand(sandboxcmd.NewStartCommand(provideConfig))
	root.AddCommand(sandboxcmd.NewStopCommand(provideConfig))
	root.AddCommand(newVersionCommand())
	classifyArguments(root)
	return root, nil
}

// usageArgs classifies positional validation failures as usage errors.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(command *cobra.Command, args []string) error {
		if err := validate(command, args); err != nil {
			return &codedError{err: err, code: exitUsage}
		}
		return nil
	}
}

// classifyArguments applies usage classification throughout the registered command tree.
func classifyArguments(command *cobra.Command) {
	if command.Args != nil {
		command.Args = usageArgs(command.Args)
	}
	for _, child := range command.Commands() {
		classifyArguments(child)
	}
}

// errorExitCode groups domain error codes into the CLI's documented failure statuses.
func errorExitCode(err error) int {
	code, ok := errdefs.CodeOf(err)
	if !ok {
		return 1
	}
	switch code {
	case errdefs.CodeNotFound:
		return 3
	case errdefs.CodeNameTaken, errdefs.CodeStateConflict, errdefs.CodeReferenced:
		return 4
	case errdefs.CodeInvalidArgument, errdefs.CodeHostIncompatible, errdefs.CodeImageIncompatible, errdefs.CodeDigestMismatch, errdefs.CodeArtifactCorrupt:
		return 5
	case errdefs.CodeArtifactUnavailable, errdefs.CodeStoreBusy:
		return 6
	default:
		return 1
	}
}

// newVersionCommand exposes build metadata as human-readable text or JSON.
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
