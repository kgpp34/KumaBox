package image

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/images"
)

// newListCommand renders catalog entries as an aligned table or detailed JSON.
func newListCommand(roots rootsProvider) *cobra.Command {
	asJSON := false
	command := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list imported images",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (returnErr error) {
			state, err := core.OpenImages(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.Close()) }()
			items, err := state.Catalog.List(command.Context())
			if err != nil {
				return err
			}
			if asJSON {
				results := make([]imageOutput, 0, len(items))
				for _, item := range items {
					results = append(results, imageResult(item))
				}
				return writeJSON(command.OutOrStdout(), results)
			}
			return writeImagesTable(command.OutOrStdout(), items)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "write JSON")
	return command
}

// newInspectCommand resolves a name or digest and preserves full metadata in JSON.
func newInspectCommand(roots rootsProvider) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect IMAGE",
		Short: "inspect an imported image",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			state, err := core.OpenImages(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.Close()) }()
			image, err := state.Catalog.Resolve(command.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(command.OutOrStdout(), imageResult(image))
		},
	}
}

// newVerifyCommand checks persisted artifacts and reports waiting on stderr.
// Store cleanup completes before the progress reporter emits its final status.
func newVerifyCommand(roots rootsProvider) *cobra.Command {
	return &cobra.Command{
		Use:   "verify IMAGE",
		Short: "verify image artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			progress, err := startImageProgress(command, "Verify", args[0])
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			if err := progress.Status("checking image artifacts"); err != nil {
				return err
			}
			state, err := core.OpenImages(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.Close()) }()
			image, err := images.Verify(command.Context(), state.Paths, state.Catalog, args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(progress.Output(command.OutOrStdout()), "verified %s\n", image.ManifestDigest)
			return err
		},
	}
}
