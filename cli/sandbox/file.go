package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/cli/stdio"

	"github.com/kumabox/kumabox/client"
	"github.com/kumabox/kumabox/types"
)

// NewFileCommand operates on guest files. The existing fs command continues
// to manage virtio-fs attachments rather than the contents of guest mounts.
func NewFileCommand(configuration configProvider) *cobra.Command {
	command := &cobra.Command{Use: "file", Short: "read and manage files inside a running sandbox", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	command.AddCommand(newFileList(configuration), newFileRead(configuration), newFileWrite(configuration), newFileStat(configuration))
	command.AddCommand(newFileMutation(configuration, "mkdir"), newFileMutation(configuration, "rm"), newFileMutation(configuration, "mv"))
	return command
}

func validateFilePath(path string, destructive bool) error {
	if err := types.ValidateGuestPath(path, destructive); err != nil {
		return invalidFlag("path", err)
	}
	return nil
}

func newFileRead(configuration configProvider) *cobra.Command {
	return &cobra.Command{
		Use: "read SANDBOX PATH", Short: "stream a regular guest file to stdout", Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateFilePath(args[1], false); err != nil {
				return err
			}
			return withGuestService(command, configuration, func(service *client.SandboxService) error {
				return service.ReadFile(command.Context(), args[0], args[1], command.OutOrStdout())
			})
		},
	}
}

func newFileStat(configuration configProvider) *cobra.Command {
	return &cobra.Command{
		Use: "stat SANDBOX PATH", Short: "print guest entry metadata as indented JSON", Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateFilePath(args[1], false); err != nil {
				return err
			}
			return withGuestService(command, configuration, func(service *client.SandboxService) error {
				info, err := service.StatFile(command.Context(), args[0], args[1])
				if err != nil {
					return err
				}
				return writeGuestJSON(command.OutOrStdout(), info)
			})
		},
	}
}

func newFileWrite(configuration configProvider) *cobra.Command {
	var parents bool
	var mode string
	command := &cobra.Command{
		Use: "write SANDBOX PATH [SOURCE|-]", Short: "atomically replace a guest file from a local file or stdin", Args: cobra.RangeArgs(2, 3),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateFilePath(args[1], true); err != nil {
				return err
			}
			options := types.FileWriteOptions{Parents: parents}
			if command.Flags().Changed("mode") {
				parsed, err := strconv.ParseUint(mode, 8, 32)
				if err != nil || parsed > 0o777 {
					return invalidFlag("mode", errors.New("expected octal permission bits 0000..0777"))
				}
				value := uint32(parsed)
				options.Mode = &value
			}
			return withGuestService(command, configuration, func(service *client.SandboxService) error {
				source := "-"
				if len(args) > 2 {
					source = args[2]
				}
				input, err := fileInput(command, source)
				if err != nil {
					return err
				}
				defer input.Close() //nolint:errcheck // read-only source; transfer also owns cancellation
				return service.WriteFile(command.Context(), args[0], args[1], options, input)
			})
		},
	}
	command.Flags().BoolVarP(&parents, "parents", "p", false, "create missing parent directories")
	command.Flags().StringVar(&mode, "mode", "", "octal permission bits; default preserves existing mode or uses 0600")
	return command
}

func fileInput(command *cobra.Command, source string) (io.ReadCloser, error) {
	if source != "-" {
		return os.Open(source) //nolint:gosec // explicit owner-selected local upload source
	}
	return stdio.Input(command.InOrStdin())
}

func newFileMutation(configuration configProvider, verb string) *cobra.Command {
	var parents, recursive bool
	command := &cobra.Command{
		Use: verb + " SANDBOX PATH", Short: "manage a guest filesystem entry", Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateFilePath(args[1], verb != "mkdir"); err != nil {
				return err
			}
			if verb == "mv" {
				if err := validateFilePath(args[2], true); err != nil {
					return err
				}
			}
			return withGuestService(command, configuration, func(service *client.SandboxService) error {
				switch verb {
				case "mkdir":
					return service.MakeDirectory(command.Context(), args[0], args[1], parents)
				case "rm":
					return service.RemoveFile(command.Context(), args[0], args[1], recursive)
				default:
					return service.RenameFile(command.Context(), args[0], args[1], args[2])
				}
			})
		},
	}
	switch verb {
	case "mkdir":
		command.Short = "create a guest directory with 0700 permissions"
		command.Flags().BoolVarP(&parents, "parents", "p", false, "create parent directories; allow an existing directory")
	case "rm":
		command.Short = "remove a guest file or empty directory"
		command.Flags().BoolVarP(&recursive, "recursive", "r", false, "remove a directory tree (does not follow final symlinks)")
	case "mv":
		command.Use, command.Args = "mv SANDBOX FROM TO", cobra.ExactArgs(3)
		command.Short = "rename a guest entry; may replace an existing target"
	}
	return command
}

func newFileList(configuration configProvider) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use: "ls SANDBOX PATH", Aliases: []string{"list"}, Short: "stream directory entries in filesystem order", Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateFilePath(args[1], false); err != nil {
				return err
			}
			return withGuestService(command, configuration, func(service *client.SandboxService) error {
				return printFileList(command.OutOrStdout(), asJSON, func(visit func(types.FileInfo) error) error {
					return service.ListFiles(command.Context(), args[0], args[1], visit)
				})
			})
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print a JSON array (nonzero exit means it is incomplete)")
	return command
}

// printFileList flushes table batches and writes JSON entries individually, so
// presentation cannot turn a streamed directory listing into an unbounded slice.
func printFileList(output io.Writer, asJSON bool, list func(func(types.FileInfo) error) error) error {
	if asJSON {
		if _, err := io.WriteString(output, "[\n"); err != nil {
			return err
		}
		separator := ""
		err := list(func(info types.FileInfo) error {
			data, err := json.MarshalIndent(info, "  ", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(output, "%s  %s", separator, data)
			separator = ",\n"
			return err
		})
		_, endErr := io.WriteString(output, "\n]\n")
		return errors.Join(err, endErr)
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "MODE\tTYPE\tSIZE\tMODIFIED\tNAME"); err != nil {
		return err
	}
	count := 0
	err := list(func(info types.FileInfo) error {
		if _, err := fmt.Fprintf(table, "%04o\t%s\t%d\t%s\t%q\n", info.Mode, info.Kind, info.Size, info.ModifiedAt.Format("2006-01-02T15:04:05Z"), info.Name); err != nil {
			return err
		}
		count++
		if count%128 == 0 {
			return table.Flush()
		}
		return nil
	})
	return errors.Join(err, table.Flush())
}
