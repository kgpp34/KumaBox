package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/images/oci"
	"github.com/kumabox/kumabox/metadata"
	metadatasqlite "github.com/kumabox/kumabox/metadata/sqlite"
	"github.com/kumabox/kumabox/storage"
)

type rootsProvider func() storage.Roots

func NewCommand(roots rootsProvider) *cobra.Command {
	command := &cobra.Command{Use: "image", Short: "manage OCI images", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	command.AddCommand(
		newPullCommand(roots),
		newImportCommand(roots),
		newListCommand(roots),
		newInspectCommand(roots),
		newVerifyCommand(roots),
		newRemoveCommand(roots),
	)
	return command
}

type opened struct {
	store   metadata.Store
	paths   images.Paths
	catalog *images.MetadataCatalog
}

func openStore(ctx context.Context, roots storage.Roots) (*opened, error) {
	paths, err := images.NewPaths(roots)
	if err != nil {
		return nil, err
	}
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	store, err := metadatasqlite.Open(ctx, paths.MetadataDB(), images.Collections(), metadatasqlite.DefaultOptions())
	if err != nil {
		return nil, err
	}
	return &opened{store: store, paths: paths, catalog: images.NewMetadataCatalog(store)}, nil
}

func newImporter(ctx context.Context, state *opened, stderr io.Writer, platform images.Platform) (*images.Importer, error) {
	options := images.DefaultOptions()
	converter, err := oci.NewEROFSConverterWithLimits(ctx, platform.Architecture, options.Limits)
	if err != nil {
		return nil, err
	}
	return images.NewImporter(state.paths, state.catalog, converter, textReporter{writer: stderr}, options)
}

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
			source, name, err := oci.NewRegistry(args[0])
			if err != nil {
				return err
			}
			state, err := openStore(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.store.Close()) }()
			importer, err := newImporter(command.Context(), state, command.ErrOrStderr(), parsedPlatform)
			if err != nil {
				return err
			}
			image, err := importer.Import(command.Context(), name, parsedPlatform, source)
			if err != nil {
				return err
			}
			return writeImage(command.OutOrStdout(), image)
		},
	}
	command.Flags().StringVar(&platform, "platform", platform, "target platform (linux/amd64 or linux/arm64)")
	return command
}

func newImportCommand(roots rootsProvider) *cobra.Command {
	platform := defaultPlatform()
	command := &cobra.Command{
		Use:   "import NAME PATH",
		Short: "import an OCI image layout or OCI archive",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			parsedPlatform, err := parsePlatform(platform)
			if err != nil {
				return err
			}
			state, err := openStore(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.store.Close()) }()
			source, cleanup, err := localSource(command.Context(), args[1], state.paths)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, cleanup()) }()
			importer, err := newImporter(command.Context(), state, command.ErrOrStderr(), parsedPlatform)
			if err != nil {
				return err
			}
			image, err := importer.Import(command.Context(), args[0], parsedPlatform, source)
			if err != nil {
				return err
			}
			return writeImage(command.OutOrStdout(), image)
		},
	}
	command.Flags().StringVar(&platform, "platform", platform, "target platform (linux/amd64 or linux/arm64)")
	return command
}

func newListCommand(roots rootsProvider) *cobra.Command {
	asJSON := false
	command := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "list imported images",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (returnErr error) {
			state, err := openStore(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.store.Close()) }()
			items, err := state.catalog.List(command.Context())
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
			state, err := openStore(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.store.Close()) }()
			image, err := state.catalog.Resolve(command.Context(), args[0])
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
			state, err := openStore(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.store.Close()) }()
			image, err := images.Verify(command.Context(), state.paths, state.catalog, args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "verified %s\n", image.ManifestDigest)
			return err
		},
	}
}

func newRemoveCommand(roots rootsProvider) *cobra.Command {
	return &cobra.Command{
		Use:     "remove IMAGE...",
		Aliases: []string{"rm"},
		Short:   "remove an imported image",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			state, err := openStore(command.Context(), roots())
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, state.store.Close()) }()
			for _, reference := range args {
				removed, err := images.Remove(command.Context(), state.paths, state.catalog, reference)
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

func localSource(ctx context.Context, path string, paths images.Paths) (images.Source, func() error, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect OCI source: %w", err)
	}
	if info.IsDir() {
		source, err := oci.NewLayout(path)
		return source, func() error { return nil }, err
	}
	source, cleanup, err := oci.NewArchiveContext(ctx, path, paths.StagingDir(), oci.DefaultLimits())
	return source, cleanup, err
}

func parsePlatform(value string) (images.Platform, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] != "linux" || (parts[1] != "amd64" && parts[1] != "arm64") {
		return images.Platform{}, errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("unsupported platform %q", value))
	}
	return images.Platform{OS: parts[0], Architecture: parts[1]}, nil
}

func defaultPlatform() string { return "linux/" + runtime.GOARCH }

func writeImage(writer io.Writer, image images.Image) error {
	_, err := fmt.Fprintf(writer, "%s\t%s\n", strings.Join(image.Names, ","), image.ManifestDigest)
	return err
}

type textReporter struct {
	writer io.Writer
}

func (r textReporter) Layer(position, total int, digest images.Digest) error {
	_, err := fmt.Fprintf(r.writer, "layer %d/%d %s\n", position+1, total, digest)
	return err
}

func (r textReporter) Committed(images.Image) error { return nil }
