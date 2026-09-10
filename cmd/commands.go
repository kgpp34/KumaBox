// Package cmd defines the kumabox command tree.
//
// This package owns flags, help and exit codes only: every command converts its
// flags into a typed request, calls a library, and renders the result. No
// lifecycle step order lives here (docs/ARCHITECTURE.md §1).
package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/cmd/doctor"
	"github.com/kumabox/kumabox/version"
)

// Execute runs one invocation and returns the process exit code.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := newRootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	if err := root.ExecuteContext(ctx); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "kumabox",
		Short: "microVM sandboxes for AI agents",
		Long: "kumabox runs microVM sandboxes on this machine.\n\n" +
			"Every command opens the node root, does one job and exits; there is no\n" +
			"daemon in this version. Use --root to point at a different root, for\n" +
			"example while developing.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("root", "",
		"node root directory (default $KUMABOX_ROOT, then /var/lib/kumabox)")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	})

	root.AddCommand(doctor.NewCommand())
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
				return writeJSON(command.OutOrStdout(), version.Info())
			}
			fmt.Fprintln(command.OutOrStdout(), version.String())
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the version as JSON")
	return command
}
