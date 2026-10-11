package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

// NewRestoreCommand builds the top-level native snapshot restore command.
func NewRestoreCommand(configuration configProvider) *cobra.Command {
	var asJSON bool
	var fromDir string
	var force bool
	var pull bool
	command := &cobra.Command{
		Use:   "restore SANDBOX [SNAPSHOT]",
		Short: "restore a sandbox to a saved snapshot",
		Args: func(command *cobra.Command, args []string) error {
			if err := cobra.RangeArgs(1, 2)(command, args); err != nil {
				return err
			}
			if (len(args) == 1) == (fromDir == "") {
				return invalidFlag("from-dir", errors.New("provide exactly one of SNAPSHOT or --from-dir"))
			}
			if force && fromDir == "" {
				return invalidFlag("force", errors.New("requires --from-dir"))
			}
			return nil
		},
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			reference := ""
			if len(args) == 2 {
				reference = args[1]
			}
			progress, err := startRestoreProgress(command, args[0])
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			service, err := core.OpenSnapshots(command.Context(), configuration(), progress)
			if err != nil {
				return err
			}
			committed := false
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.WithContext(service.Close(), errdefs.ContextInfo{
					Operation: "restore sandbox",
					Entity:    args[0],
					Phase:     "close metadata",
					Action:    "inspect the sandbox before retrying",
					Committed: committed,
				}))
			}()
			record, err := service.RestoreWithOptions(command.Context(), args[0], reference, core.RestoreOptions{SourceDirectory: fromDir, Force: force, Pull: pull})
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.WithContext(err, errdefs.ContextInfo{
					Operation: "restore sandbox",
					Entity:    args[0],
					Phase:     "output",
					Action:    "sandbox is running; inspect it before retrying",
					Committed: true,
				})
			}
			return nil
		},
	}
	command.Flags().StringVar(&fromDir, "from-dir", "", "restore from a portable snapshot directory")
	command.Flags().BoolVar(&force, "force", false, "allow a directory snapshot from another sandbox with a compatible image and resource shape")
	command.Flags().BoolVar(&pull, "pull", false, "pull the snapshot's pinned registry image if absent")
	command.Flags().BoolVar(&asJSON, "json", false, "print the restored sandbox as indented JSON")
	return command
}
