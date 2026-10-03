package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
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

// NewFSCommand groups runtime virtio-fs attachment and inspection.
func NewFSCommand(configuration configProvider) *cobra.Command {
	command := &cobra.Command{Use: "fs", Short: "attach, detach, or list virtio-fs shares", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	var socket, tag string
	var numQueues, queueSize int
	var asJSON bool
	attach := &cobra.Command{
		Use: "attach SANDBOX", Short: "attach an existing virtiofsd socket to a running sandbox", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if socket == "" {
				return invalidFlag("socket", errors.New("is required"))
			}
			if tag == "" {
				return invalidFlag("tag", errors.New("is required"))
			}
			share, err := types.NormalizeFileShare(types.FileShare{Socket: socket, Tag: tag, NumQueues: numQueues, QueueSize: queueSize})
			if err != nil {
				return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, err)
			}
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			devices, err := service.AttachFileShare(command.Context(), args[0], share)
			if err != nil {
				return err
			}
			return writeAttachedDevices(command.OutOrStdout(), devices, asJSON, "fs "+tag+" attached")
		},
	}
	attach.Flags().StringVar(&socket, "socket", "", "absolute path to an existing virtiofsd Unix socket")
	attach.Flags().StringVar(&tag, "tag", "", "guest mount tag and detach key")
	attach.Flags().IntVar(&numQueues, "num-queues", 0, "request queues (default 1)")
	attach.Flags().IntVar(&queueSize, "queue-size", 0, "queue depth (default 1024)")
	attach.Flags().BoolVar(&asJSON, "json", false, "print live devices as indented JSON")
	var detachTag string
	detach := &cobra.Command{
		Use: "detach SANDBOX", Short: "eject a runtime virtio-fs share", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			if detachTag == "" {
				return invalidFlag("tag", errors.New("is required"))
			}
			if err := types.ValidateFileShareTag(detachTag); err != nil {
				return invalidFlag("tag", err)
			}
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			devices, err := service.DetachFileShare(command.Context(), args[0], detachTag)
			if err != nil {
				return err
			}
			return writeAttachedDevices(command.OutOrStdout(), devices, asJSON, "fs "+detachTag+" detached")
		},
	}
	detach.Flags().StringVar(&detachTag, "tag", "", "guest mount tag to detach")
	detach.Flags().BoolVar(&asJSON, "json", false, "print live devices as indented JSON")
	var listJSON bool
	list := &cobra.Command{
		Use: "list SANDBOX", Short: "list runtime virtio-fs shares", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			service, err := core.OpenSandbox(command.Context(), configuration(), nil)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, service.Close()) }()
			devices, err := service.AttachedDevices(command.Context(), args[0])
			if err != nil {
				return err
			}
			if listJSON {
				shares := devices.FS
				if shares == nil {
					shares = []types.AttachedFileShare{}
				}
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(shares)
			}
			for _, share := range devices.FS {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\n", share.Tag, share.Socket); err != nil {
					return err
				}
			}
			return nil
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "print file shares as indented JSON")
	command.AddCommand(attach, detach, list)
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
	for _, share := range devices.FS {
		if _, err := fmt.Fprintf(output, "fs %s: %s\n", share.Tag, share.Socket); err != nil {
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
