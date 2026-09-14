package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/images"
)

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
				return json.NewEncoder(command.OutOrStdout()).Encode(results)
			}
			for _, item := range items {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%d\n", strings.Join(item.Names, ","), item.ManifestDigest, item.Size); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "write JSON")
	return command
}

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
			return json.NewEncoder(command.OutOrStdout()).Encode(imageResult(image))
		},
	}
}

func newVerifyCommand(roots rootsProvider) *cobra.Command {
	return &cobra.Command{
		Use:   "verify IMAGE",
		Short: "verify image artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			state, err := core.OpenImages(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.Close()) }()
			image, err := images.Verify(command.Context(), state.Paths, state.Catalog, args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "verified %s\n", image.ManifestDigest)
			return err
		},
	}
}
