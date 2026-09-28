package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
)

// newExportCommand streams one immutable snapshot to an atomically published
// file, or directly to stdout when --output=- is requested.
func newExportCommand(configuration configProvider) *cobra.Command {
	var output string
	var toDir string
	var compress bool
	command := &cobra.Command{
		Use:   "export SNAPSHOT",
		Short: "export a portable snapshot archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			progress, err := newOperationProgress(command, "Export", args[0])
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			service, err := core.OpenSnapshots(command.Context(), configuration(), progress)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			if toDir != "" {
				if err := exportDirectory(command, service, args[0], toDir); err != nil {
					return err
				}
				_, err := fmt.Fprintln(progress.Output(command.OutOrStdout()), toDir)
				return err
			}
			if output == "-" {
				_, err := service.Export(command.Context(), args[0], command.OutOrStdout(), compress)
				return err
			}
			if output == "" {
				record, err := service.Inspect(command.Context(), args[0])
				if err != nil {
					return err
				}
				output = record.ID.String() + ".tar"
				if compress {
					output += ".gz"
				}
			}
			if err := exportFile(command, service, args[0], output, compress); err != nil {
				return err
			}
			_, err = fmt.Fprintln(progress.Output(command.OutOrStdout()), output)
			return err
		},
	}
	command.Flags().StringVarP(&output, "output", "o", "", "archive file (default: snapshot ID.tar; - writes to stdout)")
	command.Flags().StringVar(&toDir, "to-dir", "", "export to an absent directory for direct clone or restore")
	command.Flags().BoolVar(&compress, "gzip", false, "compress the archive with gzip")
	command.MarkFlagsMutuallyExclusive("to-dir", "output")
	command.MarkFlagsMutuallyExclusive("to-dir", "gzip")
	return command
}

func exportDirectory(command *cobra.Command, service *core.SnapshotService, reference, destination string) (returnErr error) {
	stage, err := os.MkdirTemp(filepath.Dir(destination), ".kumabox-snapshot-*")
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(stage)) }()
	if _, err := service.ExportDirectory(command.Context(), reference, stage); err != nil {
		return err
	}
	if err := storage.PublishDir(stage, destination); err != nil {
		return err
	}
	return nil
}

func exportFile(command *cobra.Command, service *core.SnapshotService, reference, destination string, compress bool) (returnErr error) {
	parent := filepath.Dir(destination)
	file, err := os.CreateTemp(parent, ".kumabox-snapshot-*")
	if err != nil {
		return fmt.Errorf("create temporary archive: %w", err)
	}
	staged := file.Name()
	defer func() { returnErr = errors.Join(returnErr, os.Remove(staged)) }()
	if err := file.Chmod(0o600); err != nil {
		return errors.Join(err, file.Close())
	}
	_, exportErr := service.Export(command.Context(), reference, file, compress)
	if exportErr != nil {
		return errors.Join(exportErr, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	// Link is an atomic no-replace publication on the same output filesystem.
	if err := os.Link(staged, destination); err != nil {
		return fmt.Errorf("publish archive %s: %w", destination, err)
	}
	directory, err := os.Open(parent) //nolint:gosec // caller-selected output directory is opened only for fsync
	if err != nil {
		return errdefs.Context(err, "export snapshot", reference, "sync output", "archive exists; inspect it before retrying", true)
	}
	return errdefs.Context(errors.Join(directory.Sync(), directory.Close()), "export snapshot", reference, "sync output", "archive exists; inspect it before retrying", true)
}

// newImportCommand accepts a file or stdin and reports the fresh snapshot ID.
func newImportCommand(configuration configProvider) *cobra.Command {
	var name, description string
	var asJSON bool
	command := &cobra.Command{
		Use:   "import [FILE]",
		Short: "import a portable tar or tar.gz snapshot",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			reference := "stdin"
			input := command.InOrStdin()
			if len(args) == 1 && args[0] != "-" {
				reference = args[0]
				file, err := os.Open(args[0]) //nolint:gosec // requested local archive path
				if err != nil {
					return err
				}
				defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
				input = file
			}
			progress, err := newOperationProgress(command, "Import", reference)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			service, err := core.OpenSnapshots(command.Context(), configuration(), progress)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			record, err := service.Import(command.Context(), input, name, description)
			if err != nil {
				return err
			}
			return writeResult(progress.Output(command.OutOrStdout()), record, asJSON)
		},
	}
	command.Flags().StringVar(&name, "name", "", "override the archive's snapshot name")
	command.Flags().StringVar(&description, "description", "", "override the archive's description")
	command.Flags().BoolVar(&asJSON, "json", false, "print the imported snapshot as indented JSON")
	return command
}
