package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/batch"
	"github.com/kumabox/kumabox/internal/config"
	"github.com/kumabox/kumabox/internal/resources"
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
	var name, fromDirectory string
	cmd := &cobra.Command{
		Use: "import [PACKAGE]", Short: "Import a snapshot package or directory", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && fromDirectory == "" {
				return fmt.Errorf("provide PACKAGE or --from-dir")
			}
			if len(args) != 0 && fromDirectory != "" {
				return fmt.Errorf("PACKAGE and --from-dir are mutually exclusive")
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			input := fromDirectory
			if len(args) != 0 {
				input = args[0]
			}
			input, err = filepath.Abs(input)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			if stores.Metadata != nil {
				defer func() { _ = stores.Metadata.Close() }()
			}
			mutation, err := stores.Guard.BeginMutation(cmd.Context())
			if err != nil {
				return err
			}
			defer mutation.Release() //nolint:errcheck
			var rec *snapshot.Record
			if fromDirectory != "" {
				rec, err = stores.Snapshots.ImportDirectory(cmd.Context(), input, name, cfg.Storage.QEMUImgBinary)
			} else {
				rec, err = stores.Snapshots.Import(cmd.Context(), snapshot.ImportOptions{Input: input, Name: name, QEMUImgBinary: cfg.Storage.QEMUImgBinary})
			}
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "imported snapshot name")
	cmd.Flags().StringVar(&fromDirectory, "from-dir", "", "unpacked snapshot directory")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newSnapshotExportCommand(opts *rootOptions) *cobra.Command {
	var output, toDirectory, compression string
	cmd := &cobra.Command{
		Use: "export SNAPSHOT", Short: "Export a portable snapshot package", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" && toDirectory == "" {
				return fmt.Errorf("provide --output or --to-dir")
			}
			if output != "" && toDirectory != "" {
				return fmt.Errorf("--output and --to-dir are mutually exclusive")
			}
			if toDirectory != "" && cmd.Flags().Changed("compression") {
				return fmt.Errorf("--compression cannot be used with --to-dir")
			}
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			destination := output
			if toDirectory != "" {
				destination = toDirectory
			}
			absolute, err := filepath.Abs(destination)
			if err != nil {
				return fmt.Errorf("resolve export output: %w", err)
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			if stores.Metadata != nil {
				defer func() { _ = stores.Metadata.Close() }()
			}
			if toDirectory != "" {
				if err := stores.Snapshots.ExportDirectory(cmd.Context(), args[0], absolute); err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), map[string]string{"snapshot": args[0], "directory": absolute})
			}
			if err := stores.Snapshots.Export(cmd.Context(), args[0], snapshot.ExportOptions{Output: absolute, Compression: compression}); err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), map[string]string{"snapshot": args[0], "output": absolute, "compression": compression})
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "output .kbsnap path")
	cmd.Flags().StringVar(&toDirectory, "to-dir", "", "output unpacked snapshot directory")
	cmd.Flags().StringVar(&compression, "compression", "none", "compression: none, gzip, or zstd")
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
	var concurrency int
	cmd := &cobra.Command{
		Use: "rm SNAPSHOT...", Aliases: []string{"remove"}, Short: "Remove unused snapshots", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateBatchConcurrency(concurrency); err != nil {
				return err
			}
			args = batch.Distinct(args)
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			stores, err := configuredStores(cfg)
			if err != nil {
				return err
			}
			mutation, err := stores.Guard.BeginMutation(cmd.Context())
			if err != nil {
				return err
			}
			defer mutation.Release() //nolint:errcheck
			result := batch.Run(cmd.Context(), args, batch.Options{Concurrency: concurrency}, "remove snapshot", func(ctx context.Context, _ int, ref string) (*snapshot.Record, error) {
				return removeSnapshot(ctx, stores, ref)
			})
			return writeResourceBatchResult(func(value any) error {
				return writeJSON(cmd.OutOrStdout(), value)
			}, args, "remove snapshot", result)
		},
	}
	addResourceBatchConcurrencyFlag(cmd, &concurrency)
	return cmd
}

func removeSnapshot(ctx context.Context, stores resources.StoreSet, ref string) (*snapshot.Record, error) {
	if stores.References != nil {
		record, err := stores.Snapshots.Inspect(ref)
		if err != nil {
			return nil, err
		}
		refs, err := stores.References.ListTarget(ctx, "snapshot", record.ID)
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			return nil, fmt.Errorf("SNAPSHOT_IN_USE: snapshot %s has %d explicit reference(s)", record.Name, len(refs))
		}
	}
	record, err := stores.Snapshots.Remove(ref)
	if err != nil {
		return nil, err
	}
	if stores.References != nil {
		if err := stores.References.DeleteSource(ctx, "snapshot", record.ID); err != nil {
			return nil, fmt.Errorf("remove snapshot references: %w", err)
		}
	}
	return record, nil
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
