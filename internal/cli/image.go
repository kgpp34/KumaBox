// SPDX-License-Identifier: MIT

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
	cmd.AddCommand(newImageImportCommand(opts))
	cmd.AddCommand(newImagePullCommand(opts))
	cmd.AddCommand(newImageLSCommand(opts))
	cmd.AddCommand(newImageInspectCommand(opts))
	return cmd
}

func newImageImportCommand(opts *rootOptions) *cobra.Command {
	var name string
	var firmware string
	var qemuImg string

	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Import a local cloud image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rec, err := imagestore.New(cfg.Runtime.RootDir).ImportLocal(imagestore.ImportRequest{
				Name:        name,
				File:        args[0],
				Firmware:    firmware,
				QemuImgPath: qemuImg,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name")
	cmd.Flags().StringVar(&firmware, "firmware", "", "UEFI firmware path")
	cmd.Flags().StringVar(&qemuImg, "qemu-img", "qemu-img", "qemu-img binary path")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("firmware")
	return cmd
}

func newImagePullCommand(opts *rootOptions) *cobra.Command {
	var name string
	var firmware string
	var qemuImg string
	var sha256Digest string

	cmd := &cobra.Command{
		Use:   "pull URL",
		Short: "Pull a cloud image URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			rec, err := imagestore.New(cfg.Runtime.RootDir).Pull(imagestore.PullRequest{
				Name:        name,
				URL:         args[0],
				Firmware:    firmware,
				QemuImgPath: qemuImg,
				SHA256:      sha256Digest,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name")
	cmd.Flags().StringVar(&firmware, "firmware", "", "UEFI firmware path")
	cmd.Flags().StringVar(&qemuImg, "qemu-img", "qemu-img", "qemu-img binary path")
	cmd.Flags().StringVar(&sha256Digest, "sha256", "", "expected image sha256 digest")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("firmware")
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
