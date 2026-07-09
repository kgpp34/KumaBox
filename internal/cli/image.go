// SPDX-License-Identifier: MIT

package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/internal/imagestore"
	"github.com/kumabox/kumabox/internal/ociresolver"
	"github.com/kumabox/kumabox/internal/ocistore"
	"github.com/kumabox/kumabox/internal/vmstore"
)

func newImageCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage KumaBox images",
	}
	cmd.AddCommand(newImageImportCommand(opts))
	cmd.AddCommand(newImagePullCommand(opts))
	cmd.AddCommand(newImagePullOCICommand(opts))
	cmd.AddCommand(newImageBuildCommand(opts))
	cmd.AddCommand(newImageLSCommand(opts))
	cmd.AddCommand(newImageInspectCommand(opts))
	cmd.AddCommand(newImageRMCommand(opts))
	return cmd
}

func newImagePullOCICommand(opts *rootOptions) *cobra.Command {
	var platform string
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "pull-oci REF",
		Short: "Pull OCI blobs into the content store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			result, err := ocistore.New(cfg.Runtime.RootDir).Pull(cmd.Context(), ocistore.PullRequest{
				Ref:      args[0],
				Platform: platform,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&platform, "platform", ociresolver.DefaultPlatform(), "OCI platform os/arch[/variant]")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newImageBuildCommand(opts *rootOptions) *cobra.Command {
	var name string
	var platform string
	var dryRun bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "build REF",
		Short: "Resolve an OCI VM image reference",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := loadConfig(opts); err != nil {
				return err
			}
			if name == "" {
				return errors.New("--name is required")
			}
			if !dryRun {
				return fmt.Errorf("P3_BUILD_NOT_IMPLEMENTED: image build without --dry-run is implemented in later P3 sections")
			}
			if !jsonOutput {
				return fmt.Errorf("P3_RESOLVE_REQUIRES_JSON: P3-01 dry-run output requires --json")
			}

			result, err := (ociresolver.Resolver{}).Resolve(cmd.Context(), args[0], platform)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), struct {
				SchemaVersion string              `json:"schemaVersion"`
				Name          string              `json:"name"`
				DryRun        bool                `json:"dryRun"`
				Result        *ociresolver.Result `json:"result"`
			}{
				SchemaVersion: "kumabox.oci.resolve.v1",
				Name:          name,
				DryRun:        true,
				Result:        result,
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name")
	cmd.Flags().StringVar(&platform, "platform", ociresolver.DefaultPlatform(), "OCI platform os/arch[/variant]")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "resolve OCI metadata without publishing an image")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	_ = cmd.MarkFlagRequired("name")
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

func newImageRMCommand(opts *rootOptions) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "rm IMAGE",
		Aliases: []string{"remove"},
		Short:   "Remove an unused image",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			refs, err := imageReferencesFromVMs(cfg.Runtime.RootDir)
			if err != nil {
				return err
			}
			rec, err := imagestore.New(cfg.Runtime.RootDir).Remove(imagestore.RemoveRequest{
				Ref:        args[0],
				Force:      force,
				References: refs,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "allow removal of damaged unreferenced image directories")
	return cmd
}

func imageReferencesFromVMs(rootDir string) ([]imagestore.Reference, error) {
	records, err := vmstore.New(rootDir).List()
	if err != nil {
		return nil, err
	}
	refs := make([]imagestore.Reference, 0)
	for _, rec := range records {
		if rec == nil || rec.Image == nil {
			continue
		}
		refs = append(refs, imagestore.Reference{
			VMID:    rec.ID,
			VMName:  rec.Name,
			VMState: string(rec.State),
			ImageID: rec.Image.ID,
		})
	}
	return refs, nil
}
