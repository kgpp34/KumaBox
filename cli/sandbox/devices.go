package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/types"
)

// NewDiskCommand groups runtime external raw-disk operations.
func NewDiskCommand(configuration configProvider) *cobra.Command {
	command := &cobra.Command{Use: "disk", Short: "attach or detach an external raw disk", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	var path, name, directIO string
	var readonly, asJSON bool
	attach := &cobra.Command{
		Use: "attach SANDBOX", Short: "attach an existing raw file to a running sandbox", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if path == "" {
				return invalidFlag("path", errors.New("is required"))
			}
			if name == "" {
				return invalidFlag("name", errors.New("is required"))
			}
			if directIO != "auto" && directIO != "on" && directIO != "off" {
				return invalidFlag("directio", errors.New("must be auto, on, or off"))
			}
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			devices, err := service.AttachDisk(command.Context(), args[0], types.ExternalDisk{
				Path: path, Name: name, ReadOnly: readonly, DirectIO: directIO == "on" || (directIO == "auto" && !readonly),
			})
			if err != nil {
				return err
			}
			return writeAttachedDevices(command.OutOrStdout(), devices, asJSON, "disk "+name+" attached")
		},
	}
	attach.Flags().StringVar(&path, "path", "", "absolute path to an existing raw disk outside KumaBox roots")
	attach.Flags().StringVar(&name, "name", "", "guest disk serial and detach key")
	attach.Flags().BoolVar(&readonly, "readonly", false, "attach the disk read-only")
	attach.Flags().StringVar(&directIO, "directio", "auto", "use O_DIRECT: auto, on, or off")
	attach.Flags().BoolVar(&asJSON, "json", false, "print live devices as indented JSON")
	var detachName string
	detach := &cobra.Command{
		Use: "detach SANDBOX", Short: "eject a runtime disk without deleting its file", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if detachName == "" {
				return invalidFlag("name", errors.New("is required"))
			}
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			devices, err := service.DetachDisk(command.Context(), args[0], detachName)
			if err != nil {
				return err
			}
			return writeAttachedDevices(command.OutOrStdout(), devices, asJSON, "disk "+detachName+" detached")
		},
	}
	detach.Flags().StringVar(&detachName, "name", "", "guest disk serial to detach")
	detach.Flags().BoolVar(&asJSON, "json", false, "print live devices as indented JSON")
	command.AddCommand(attach, detach)
	return command
}

// NewDeviceCommand groups runtime PCI passthrough operations.
func NewDeviceCommand(configuration configProvider) *cobra.Command {
	command := &cobra.Command{Use: "device", Short: "attach or detach a VFIO PCI device", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	var pci, id string
	var asJSON bool
	attach := &cobra.Command{
		Use: "attach SANDBOX", Short: "assign a VFIO-bound host PCI device", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if pci == "" {
				return invalidFlag("pci", errors.New("is required"))
			}
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			devices, err := service.AttachPCIDevice(command.Context(), args[0], pci, id)
			if err != nil {
				return err
			}
			return writeAttachedDevices(command.OutOrStdout(), devices, asJSON, "PCI device attached")
		},
	}
	attach.Flags().StringVar(&pci, "pci", "", "host BDF or /sys/bus/pci/devices/<BDF> path")
	attach.Flags().StringVar(&id, "id", "", "detach key (default is derived from BDF)")
	attach.Flags().BoolVar(&asJSON, "json", false, "print live devices as indented JSON")
	var detachID string
	detach := &cobra.Command{
		Use: "detach SANDBOX", Short: "eject a passed-through PCI device", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if detachID == "" {
				return invalidFlag("id", errors.New("is required"))
			}
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			devices, err := service.DetachPCIDevice(command.Context(), args[0], detachID)
			if err != nil {
				return err
			}
			return writeAttachedDevices(command.OutOrStdout(), devices, asJSON, "PCI device "+detachID+" detached")
		},
	}
	detach.Flags().StringVar(&detachID, "id", "", "attached PCI device ID")
	detach.Flags().BoolVar(&asJSON, "json", false, "print live devices as indented JSON")
	command.AddCommand(attach, detach)
	return command
}

func writeAttachedDevices(output io.Writer, devices types.AttachedDevices, asJSON bool, message string) error {
	if asJSON {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(devices)
	}
	_, err := fmt.Fprintln(output, message)
	if err != nil {
		return err
	}
	for _, disk := range devices.Disks {
		if _, err := fmt.Fprintf(output, "disk %s: %s\n", disk.Name, disk.Path); err != nil {
			return err
		}
	}
	for _, pci := range devices.Devices {
		if _, err := fmt.Fprintf(output, "PCI %s: %s\n", pci.ID, pci.BDF); err != nil {
			return err
		}
	}
	return nil
}
