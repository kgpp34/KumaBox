package cli

import (
	"fmt"
	"github.com/kumabox/kumabox/internal/backend"
	kbruntime "github.com/kumabox/kumabox/internal/vm/runtime"
	"github.com/spf13/cobra"
)

func newDiskCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "disk", Short: "Manage runtime disks"}
	attach := &cobra.Command{Use: "attach VM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path, _ := cmd.Flags().GetString("path")
		name, _ := cmd.Flags().GetString("name")
		readonly, _ := cmd.Flags().GetBool("readonly")
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		rt, err := kbruntime.New(cfg)
		if err != nil {
			return err
		}
		rec, err := rt.AttachDisk(cmd.Context(), args[0], backend.DiskSpec{Path: path, Name: name, ReadOnly: readonly})
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), rec)
	}}
	attach.Flags().String("path", "", "absolute raw disk path")
	attach.Flags().String("name", "", "guest disk serial and detach name")
	attach.Flags().Bool("readonly", false, "attach read-only")
	_ = attach.MarkFlagRequired("path")
	_ = attach.MarkFlagRequired("name")
	detach := &cobra.Command{Use: "detach VM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			return fmt.Errorf("--name is required")
		}
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		rt, err := kbruntime.New(cfg)
		if err != nil {
			return err
		}
		rec, err := rt.DetachDisk(cmd.Context(), args[0], name)
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), rec)
	}}
	detach.Flags().String("name", "", "disk name")
	list := &cobra.Command{Use: "list VM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		rt, err := kbruntime.New(cfg)
		if err != nil {
			return err
		}
		disks, err := rt.ListDisks(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), disks)
	}}
	cmd.AddCommand(attach, detach, list)
	return cmd
}
