package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/config"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/kumabox/kumabox/internal/snapshot"
)

func newSnapshotCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "snapshot", Short: "Manage VM snapshots"}
	cmd.AddCommand(newSnapshotCreateCommand(opts))
	cmd.AddCommand(newSnapshotExportCommand(opts))
	cmd.AddCommand(newSnapshotImportCommand(opts))
	cmd.AddCommand(newSnapshotRestoreCommand(opts))
	cmd.AddCommand(newSnapshotLSCommand(opts))
	cmd.AddCommand(newSnapshotInspectCommand(opts))
	cmd.AddCommand(newSnapshotVerifyCommand(opts))
	cmd.AddCommand(newSnapshotRMCommand(opts))
	return cmd
}

func newSnapshotVerifyCommand(opts *rootOptions) *cobra.Command {
	var vmRef string
	cmd := &cobra.Command{
		Use: "verify SNAPSHOT", Short: "Verify native snapshot integrity and compatibility", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			manifest, err := rt.VerifyNativeSnapshot(cmd.Context(), args[0], vmRef)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), manifest)
		},
	}
	cmd.Flags().StringVar(&vmRef, "vm", "", "target VM used for compatibility checks")
	_ = cmd.MarkFlagRequired("vm")
	return cmd
}

func newSnapshotRestoreCommand(opts *rootOptions) *cobra.Command {
	var name string
	var cpus int
	var networks []string
	cmd := &cobra.Command{
		Use:   "restore SNAPSHOT",
		Short: "Create a new VM from a stopped snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cpus <= 0 {
				return fmt.Errorf("--cpus must be greater than zero")
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			if err := config.EnsureRuntimeDirs(cfg); err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			rec, err := rt.RestoreSnapshot(cmd.Context(), args[0], kbruntime.RestoreOptions{
				Name:     name,
				CPUs:     cpus,
				Networks: normalizedNetworkFlags(networks),
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "new VM name")
	cmd.Flags().IntVar(&cpus, "cpus", 1, "number of vCPUs")
	cmd.Flags().StringArrayVar(&networks, "network", nil, "network attachment, repeatable")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newSnapshotImportCommand(opts *rootOptions) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use: "import PACKAGE", Short: "Import an untrusted snapshot package", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			input, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			rec, err := stores.Snapshots.Import(cmd.Context(), snapshot.ImportOptions{Input: input, Name: name, QEMUImgBinary: cfg.Storage.QEMUImgBinary})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "imported snapshot name")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newSnapshotExportCommand(opts *rootOptions) *cobra.Command {
	var output, compression string
	cmd := &cobra.Command{
		Use: "export SNAPSHOT", Short: "Export a portable snapshot package", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			absolute, err := filepath.Abs(output)
			if err != nil {
				return fmt.Errorf("resolve export output: %w", err)
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			if err := stores.Snapshots.Export(cmd.Context(), args[0], snapshot.ExportOptions{Output: absolute, Compression: compression}); err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), map[string]string{"snapshot": args[0], "output": absolute, "compression": compression})
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "output .kbsnap path")
	cmd.Flags().StringVar(&compression, "compression", "none", "compression: none, gzip, or zstd")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

func newSnapshotCreateCommand(opts *rootOptions) *cobra.Command {
	var name string
	var snapshotType string
	cmd := &cobra.Command{
		Use: "create VM", Short: "Capture a stopped disk or running native snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rt, err := kbruntime.New(cfg)
			if err != nil {
				return err
			}
			var rec *snapshot.Record
			switch snapshotType {
			case "disk":
				rec, err = rt.CreateStoppedSnapshot(cmd.Context(), args[0], name)
			case "running":
				rec, err = rt.CreateRunningSnapshot(cmd.Context(), args[0], name)
			default:
				return fmt.Errorf("--type must be disk or running")
			}
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "snapshot name")
	cmd.Flags().StringVar(&snapshotType, "type", "disk", "snapshot type: disk or running")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newSnapshotLSCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "ls", Aliases: []string{"list"}, Short: "List ready snapshots",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			records, err := stores.Snapshots.List()
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			return writeSnapshotTable(cmd.OutOrStdout(), records)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newSnapshotInspectCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "inspect SNAPSHOT", Short: "Inspect a ready snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			rec, err := stores.Snapshots.Inspect(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			return writeSnapshotTable(cmd.OutOrStdout(), []*snapshot.Record{rec})
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newSnapshotRMCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use: "rm SNAPSHOT", Aliases: []string{"remove"}, Short: "Remove an unused snapshot", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			if stores.References != nil {
				record, inspectErr := stores.Snapshots.Inspect(args[0])
				if inspectErr != nil {
					return inspectErr
				}
				refs, listErr := stores.References.ListTarget(cmd.Context(), "snapshot", record.ID)
				if listErr != nil {
					return listErr
				}
				if len(refs) > 0 {
					return fmt.Errorf("SNAPSHOT_IN_USE: snapshot %s has %d explicit reference(s)", record.Name, len(refs))
				}
			}
			rec, err := stores.Snapshots.Remove(args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
}

func writeSnapshotTable(w io.Writer, records []*snapshot.Record) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tSTATE\tSIZE\tCREATED"); err != nil {
		return err
	}
	for _, rec := range records {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", rec.ID, rec.Name, rec.State, rec.SizeBytes, rec.CreatedAt.Format("2006-01-02T15:04:05Z")); err != nil {
			return err
		}
	}
	return tw.Flush()
}
