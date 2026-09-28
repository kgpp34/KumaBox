package sandbox

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
)

// NewCloneCommand builds a running sandbox from a saved native snapshot.
func NewCloneCommand(configuration configProvider) *cobra.Command {
	var name string
	var pull bool
	var fromDir string
	var nics int
	var networkName string
	var asJSON bool
	command := &cobra.Command{
		Use:   "clone [SNAPSHOT] --name NAME",
		Short: "clone a snapshot into a new running sandbox",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if name == "" {
				return invalidFlag("name", errors.New("is required"))
			}
			if (len(args) == 0) == (fromDir == "") {
				return invalidFlag("from-dir", errors.New("provide exactly one of SNAPSHOT or --from-dir"))
			}
			reference := ""
			if len(args) == 1 {
				reference = args[0]
			}
			progress, err := startCloneProgress(command, name)
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
				returnErr = errors.Join(returnErr, errdefs.Context(service.Close(), "clone sandbox", name, "close metadata", "inspect the clone before retrying", committed))
			}()
			options := core.CloneOptions{Name: name, Pull: pull, SourceDirectory: fromDir, NetworkName: networkName}
			if command.Flags().Changed("nics") {
				options.NICs = &nics
			}
			record, err := service.CloneWithOptions(command.Context(), reference, options)
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.Context(err, "clone sandbox", name, "output", "clone is running; inspect it", true)
			}
			return nil
		},
	}
	command.Flags().StringVar(&name, "name", "", "required name for the new sandbox")
	command.Flags().BoolVar(&pull, "pull", false, "pull the snapshot's pinned registry image if absent")
	command.Flags().StringVar(&fromDir, "from-dir", "", "clone from a portable snapshot directory")
	command.Flags().IntVar(&nics, "nics", 0, "override the captured NIC count, including zero")
	command.Flags().StringVar(&networkName, "network", "", "use another CNI network (default: inherit)")
	command.Flags().BoolVar(&asJSON, "json", false, "print the cloned sandbox as indented JSON")
	return command
}
