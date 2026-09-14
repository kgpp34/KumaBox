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

func newRemoveCommand(roots rootsProvider) *cobra.Command {
	return &cobra.Command{
		Use:     "remove IMAGE...",
		Aliases: []string{"rm"},
		Short:   "remove an imported image",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			state, err := core.OpenImages(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.Close()) }()
			for _, reference := range args {
				removed, err := images.Remove(command.Context(), state.Paths, state.Catalog, reference)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(command.OutOrStdout(), "removed %s\n", strings.Join(removed.Names, ",")); err != nil {
					return errdefs.Context(err, "remove image", reference, "report", "image removed", true)
				}
			}
			return nil
		},
	}
}
