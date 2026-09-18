package image

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
)

// newRemoveCommand removes references in argument order and counts completed removals.
// Reporting errors after a successful removal carry committed state so callers know
// that a failed command does not imply that the image is still present.
func newRemoveCommand(configuration configProvider) *cobra.Command {
	return &cobra.Command{
		Use:     "remove IMAGE...",
		Aliases: []string{"rm"},
		Short:   "remove an imported image",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			progress, err := startImageProgress(command, "Remove", strings.Join(args, ", "))
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			state, err := core.OpenImages(command.Context(), configuration())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.Close()) }()
			for _, reference := range args {
				if err := progress.Status("waiting for image locks and removing artifacts"); err != nil {
					return err
				}
				removed, err := images.Remove(command.Context(), state.Paths, state.Catalog, reference)
				if err != nil {
					return err
				}
				if err := progress.Removed(len(args)); err != nil {
					return errdefs.Context(err, "remove image", reference, "report", "image removed", true)
				}
				if _, err := fmt.Fprintf(progress.Output(command.OutOrStdout()), "removed %s\n", strings.Join(removed.Names, ",")); err != nil {
					return errdefs.Context(err, "remove image", reference, "report", "image removed", true)
				}
			}
			return nil
		},
	}
}
