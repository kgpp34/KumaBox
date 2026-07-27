package cli

import (
	"fmt"
	"github.com/kumabox/kumabox/internal/backend"
	kbruntime "github.com/kumabox/kumabox/internal/runtime"
	"github.com/spf13/cobra"
)

func newPCIDeviceCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "device", Short: "Manage VFIO PCI devices"}
	attach := &cobra.Command{Use: "attach VM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		pci, _ := cmd.Flags().GetString("pci")
		id, _ := cmd.Flags().GetString("id")
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		rt, err := kbruntime.New(cfg)
		if err != nil {
			return err
		}
		rec, err := rt.AttachPCIDevice(cmd.Context(), args[0], backend.PCIDeviceSpec{PCI: pci, ID: id})
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), rec)
	}}
	attach.Flags().String("pci", "", "PCI BDF or sysfs path")
	attach.Flags().String("id", "", "device id")
	_ = attach.MarkFlagRequired("pci")
	detach := &cobra.Command{Use: "detach VM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, _ := cmd.Flags().GetString("id")
		if id == "" {
			return fmt.Errorf("--id is required")
		}
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		rt, err := kbruntime.New(cfg)
		if err != nil {
			return err
		}
		rec, err := rt.DetachPCIDevice(cmd.Context(), args[0], id)
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), rec)
	}}
	detach.Flags().String("id", "", "device id")
	list := &cobra.Command{Use: "list VM", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		rt, err := kbruntime.New(cfg)
		if err != nil {
			return err
		}
		devices, err := rt.ListPCIDevices(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), devices)
	}}
	cmd.AddCommand(attach, detach, list)
	return cmd
}
