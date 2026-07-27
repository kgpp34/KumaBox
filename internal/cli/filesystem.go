package cli

import (
	"fmt"
	"github.com/kumabox/kumabox/internal/backend"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/spf13/cobra"
)

func newFilesystemCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "fs", Short: "Manage runtime virtio-fs filesystems"}
	attach := &cobra.Command{Use: "attach VM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		socket, _ := cmd.Flags().GetString("socket")
		tag, _ := cmd.Flags().GetString("tag")
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		rt, err := kbruntime.New(cfg)
		if err != nil {
			return err
		}
		rec, err := rt.AttachFilesystem(cmd.Context(), args[0], backend.FilesystemSpec{Socket: socket, Tag: tag})
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), rec)
	}}
	attach.Flags().String("socket", "", "virtiofsd socket")
	attach.Flags().String("tag", "", "guest filesystem tag")
	_ = attach.MarkFlagRequired("socket")
	_ = attach.MarkFlagRequired("tag")
	detach := &cobra.Command{Use: "detach VM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		tag, _ := cmd.Flags().GetString("tag")
		if tag == "" {
			return fmt.Errorf("--tag is required")
		}
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		rt, err := kbruntime.New(cfg)
		if err != nil {
			return err
		}
		rec, err := rt.DetachFilesystem(cmd.Context(), args[0], tag)
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), rec)
	}}
	detach.Flags().String("tag", "", "guest filesystem tag")
	list := &cobra.Command{Use: "list VM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		rt, err := kbruntime.New(cfg)
		if err != nil {
			return err
		}
		filesystems, err := rt.ListFilesystems(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), filesystems)
	}}
	cmd.AddCommand(attach, detach, list)
	return cmd
}
