package cli

import (
	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/imagestore"
)

func newImageCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage KumaBox images",
	}
	cmd.AddCommand(newImageLSCommand(opts))
	cmd.AddCommand(newImageInspectCommand(opts))
	return cmd
}

func newImageLSCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List images",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			records, err := imagestore.New(cfg.Runtime.RootDir).List()
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), records)
			}
			return writeImageTable(cmd.OutOrStdout(), records)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newImageInspectCommand(opts *rootOptions) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "inspect IMAGE",
		Short: "Inspect an image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rec, err := imagestore.New(cfg.Runtime.RootDir).Inspect(args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), rec)
			}
			return writeImageTable(cmd.OutOrStdout(), []*imagestore.ImageRecord{rec})
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}
