package image

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
)

// newPullCommand validates a registry reference and runs the shared image importer.
// Progress finishes after the store closes so cleanup failures affect the final status.
func newPullCommand(roots rootsProvider) *cobra.Command {
	platform := defaultPlatform()
	command := &cobra.Command{
		Use:   "pull REF",
		Short: "pull an OCI image from a registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			parsedPlatform, err := parsePlatform(platform)
			if err != nil {
				return err
			}
			input, name, err := core.NewRegistrySource(args[0])
			if err != nil {
				return err
			}
			progress, err := startImageProgress(command, "Pull", name)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			state, err := core.OpenImages(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.Close()) }()
			importer, err := core.NewImageImporter(command.Context(), state, progress, parsedPlatform)
			if err != nil {
				return err
			}
			if err := progress.Status("downloading and converting layers"); err != nil {
				return err
			}
			image, err := importer.Import(command.Context(), name, parsedPlatform, input)
			if err != nil {
				return err
			}
			return writeImage(progress.Output(command.OutOrStdout()), image)
		},
	}
	command.Flags().StringVar(&platform, "platform", platform, "target platform (linux/amd64 or linux/arm64)")
	return command
}

// newImportCommand selects a local Docker or OCI source and runs the shared importer.
// Defers retain cleanup errors and finish progress after all resources are released.
//
//	validate --> open store --> stage source --> import --> write result
//	                                                     |
//	final progress <-- close store <-- clean source <----+
func newImportCommand(roots rootsProvider) *cobra.Command {
	platform := defaultPlatform()
	format := "auto"
	sourceTag := ""
	command := &cobra.Command{
		Use:   "import NAME PATH",
		Short: "import a Docker image archive or OCI layout/archive",
		Long: "Import a docker save archive or an OCI image layout/archive. " +
			"Formats are detected automatically unless --format is set. " +
			"Use --source-tag to select a Docker source image; NAME is its local KumaBox name. " +
			"docker export archives are not supported.",
		Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			parsedPlatform, err := parsePlatform(platform)
			if err != nil {
				return err
			}
			sourceOptions := core.LocalImageOptions{Format: format, SourceTag: sourceTag}
			err = sourceOptions.Validate()
			if err != nil {
				return err
			}
			progress, err := startImageProgress(command, "Import", args[0])
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			state, err := core.OpenImages(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.Close()) }()
			if err := progress.Status("reading source"); err != nil {
				return err
			}
			input, cleanup, err := state.OpenLocalSource(command.Context(), args[1], sourceOptions)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, cleanup()) }()
			importer, err := core.NewImageImporter(command.Context(), state, progress, parsedPlatform)
			if err != nil {
				return err
			}
			if err := progress.Status("converting layers"); err != nil {
				return err
			}
			image, err := importer.Import(command.Context(), args[0], parsedPlatform, input)
			if err != nil {
				return err
			}
			return writeImage(progress.Output(command.OutOrStdout()), image)
		},
	}
	command.Flags().StringVar(&platform, "platform", platform, "target platform (linux/amd64 or linux/arm64)")
	command.Flags().StringVar(&format, "format", format, "input format (auto, docker, or oci); auto detects source contents")
	command.Flags().StringVar(&sourceTag, "source-tag", sourceTag, "select a source image tag inside a Docker archive (docker save format)")
	return command
}
