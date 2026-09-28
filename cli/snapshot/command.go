// Package snapshot exposes snapshot lifecycle commands through Cobra.
package snapshot

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

type configProvider func() config.Config

// NewCommand builds the snapshot command group.
func NewCommand(configuration configProvider) *cobra.Command {
	command := &cobra.Command{Use: "snapshot", Short: "manage sandbox snapshots"}
	command.AddCommand(newSaveCommand(configuration), newListCommand(configuration), newInspectCommand(configuration), newRemoveCommand(configuration), newExportCommand(configuration), newImportCommand(configuration))
	return command
}

func newSaveCommand(configuration configProvider) *cobra.Command {
	var name, description string
	var asJSON bool
	command := &cobra.Command{
		Use:   "save SANDBOX",
		Short: "save a live snapshot of a running sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			progress, err := newProgress(command, args[0])
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
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "save snapshot", args[0], "close metadata", "inspect the snapshot before retrying", committed))
			}()
			record, err := service.Save(command.Context(), core.SaveSnapshotRequest{
				SandboxReference: args[0], Name: name, Description: description,
			})
			if err != nil {
				return err
			}
			committed = true
			return writeResult(progress.Output(command.OutOrStdout()), record, asJSON)
		},
	}
	command.Flags().StringVar(&name, "name", "", "optional unique snapshot name")
	command.Flags().StringVar(&description, "description", "", "optional snapshot description")
	command.Flags().BoolVar(&asJSON, "json", false, "print the saved snapshot as indented JSON")
	return command
}

// NewHibernateCommand builds the top-level atomic snapshot-and-stop command.
func NewHibernateCommand(configuration func() config.Config) *cobra.Command {
	var name, description string
	var asJSON bool
	command := &cobra.Command{
		Use:   "hibernate SANDBOX",
		Short: "save a snapshot and stop the sandbox at the same point",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			progress, err := newOperationProgress(command, "Hibernate", args[0])
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
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "hibernate sandbox", args[0], "close metadata", "inspect the sandbox and snapshot before retrying", committed))
			}()
			record, err := service.Hibernate(command.Context(), core.SaveSnapshotRequest{
				SandboxReference: args[0], Name: name, Description: description,
			})
			if err != nil {
				return err
			}
			committed = true
			return writeResult(progress.Output(command.OutOrStdout()), record, asJSON)
		},
	}
	command.Flags().StringVar(&name, "name", "", "optional unique snapshot name")
	command.Flags().StringVar(&description, "description", "", "optional snapshot description")
	command.Flags().BoolVar(&asJSON, "json", false, "print the saved snapshot as indented JSON")
	return command
}

func newListCommand(configuration configProvider) *cobra.Command {
	var asJSON bool
	var sandboxReference string
	command := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "list snapshots",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (returnErr error) {
			service, err := core.OpenSnapshots(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "list snapshots", "", "close metadata", "retry the query", false))
			}()
			var records []types.Snapshot
			if sandboxReference == "" {
				records, err = service.List(command.Context())
			} else {
				records, err = service.ListForSandbox(command.Context(), sandboxReference)
			}
			if err != nil {
				return err
			}
			if asJSON {
				return writeListJSON(command.OutOrStdout(), records)
			}
			return writeTable(command.OutOrStdout(), records)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print snapshots as indented JSON")
	command.Flags().StringVar(&sandboxReference, "sandbox", "", "only show snapshots belonging to this sandbox")
	return command
}

func newInspectCommand(configuration configProvider) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect SNAPSHOT",
		Short: "show detailed snapshot information as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			service, err := core.OpenSnapshots(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "inspect snapshot", args[0], "close metadata", "retry the query", false))
			}()
			record, err := service.Inspect(command.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(command.OutOrStdout(), record)
		},
	}
}

func newRemoveCommand(configuration configProvider) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "rm SNAPSHOT...",
		Short: "remove one or more snapshots",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			service, err := core.OpenSnapshots(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() {
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "remove snapshot", args[0], "close metadata", "retry snapshot removal", true))
			}()
			removed := make([]types.Snapshot, 0, len(args))
			var failures []error
			for _, reference := range args {
				record, removeErr := service.Remove(command.Context(), reference)
				if removeErr != nil {
					failures = append(failures, removeErr)
					continue
				}
				removed = append(removed, record)
				if !asJSON {
					if err := writeResult(command.OutOrStdout(), record, false); err != nil {
						return errors.Join(errors.Join(failures...), err)
					}
				}
			}
			if asJSON {
				if len(args) == 1 && len(removed) == 1 {
					return errors.Join(errors.Join(failures...), writeJSON(command.OutOrStdout(), removed[0]))
				}
				return errors.Join(errors.Join(failures...), writeListJSON(command.OutOrStdout(), removed))
			}
			return errors.Join(failures...)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the removed snapshot as indented JSON")
	return command
}
