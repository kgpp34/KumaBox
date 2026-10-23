// Package sandbox exposes sandbox lifecycle commands through Cobra.
// It owns argument parsing and terminal presentation while core owns application
// ordering and assembles concrete adapters.
package sandbox

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// configProvider reads immutable configuration only after Cobra parses flags.
type configProvider func() config.Config

// createOptions contains the resource flags shared by create and run. Keeping
// parsing here gives both commands one validation contract and one set of
// defaults.
type createOptions struct {
	backend      string
	name         string
	cpus         uint32
	cpuPolicy    types.CPUPolicy
	memory       string
	sharedMemory bool
	hugePages    bool
	mergeable    bool
	noBalloon    bool
	storageSize  string
	nics         int
	networkName  string
	dataDisks    []string
}

// defaultCreateOptions returns the public resource defaults for a new sandbox.
func defaultCreateOptions() createOptions {
	return createOptions{
		cpus: types.DefaultSandboxCPUs, memory: "1GiB", storageSize: "10GiB", nics: 1,
	}
}

// addFlags registers the resource shape accepted by sandbox creation commands.
func (o *createOptions) addFlags(command *cobra.Command) {
	command.Flags().StringVar(&o.backend, "vmm", "", "VMM backend: cloud-hypervisor or firecracker (empty: configured default)")
	addCPUFlags(command, &o.cpuPolicy)
	command.Flags().StringVar(&o.name, "name", o.name, "required sandbox name")
	command.Flags().Uint32Var(&o.cpus, "cpus", o.cpus, "number of virtual CPUs")
	command.Flags().StringVar(&o.memory, "memory", o.memory, "guest memory (for example 1GiB)")
	command.Flags().BoolVar(&o.sharedMemory, "shared-memory", false, "enable shared guest memory for runtime virtio-fs attachment")
	command.Flags().BoolVar(&o.hugePages, "hugepages", false, "use preallocated host hugetlb pages (snapshot memory restores by copy)")
	command.Flags().BoolVar(&o.mergeable, "mergeable", false, "opt private RAM into host KSM (excludes hugepages and shared-memory)")
	command.Flags().BoolVar(&o.noBalloon, "no-balloon", false, "omit the balloon device instead of reclaiming 25 percent of guest RAM")
	command.Flags().StringVar(&o.storageSize, "storage", o.storageSize, "logical sparse COW size (minimum 10GiB)")
	command.Flags().IntVar(&o.nics, "nics", o.nics, "number of network interfaces (0 disables networking)")
	command.Flags().StringVar(&o.networkName, "network", o.networkName, "CNI network name (empty selects the default)")
	command.Flags().StringArrayVar(&o.dataDisks, "data-disk", nil, "managed disk: size=20GiB[,name=db][,fstype=ext4|none][,directio=on|off|auto]; repeatable")
}

// addCPUFlags gives creation and cloning the same host-policy contract.
// Guest topology remains fixed in the captured state; these flags never resize it.
func addCPUFlags(command *cobra.Command, policy *types.CPUPolicy) {
	command.Flags().Uint32Var(&policy.Weight, "cpu-weight", 0, "CPU contention weight, 1..10000 (0: vCPU count)")
	command.Flags().Int64Var(&policy.QuotaUS, "cpu-quota-us", 0, "CPU-time budget per period (0: vCPUs times period)")
	command.Flags().Int64Var(&policy.PeriodUS, "cpu-period-us", 0, "CPU bandwidth period in microseconds (0: 100000)")
	command.Flags().Int64Var(&policy.BurstUS, "cpu-burst-us", 0, "burst credit in microseconds (0: quota; -1: disabled)")
	command.Flags().StringVar(&policy.CPUSet, "cpuset-cpus", "", "explicit host CPU list, such as 0-3,6 (empty: inherit)")
}

// request validates CLI values before any persistent service is opened.
func (o createOptions) request(imageReference string) (core.CreateSandboxRequest, error) {
	if o.cpus == 0 || o.cpus > types.MaxSandboxCPUs {
		return core.CreateSandboxRequest{}, invalidFlag("cpus", fmt.Errorf("must be between 1 and %d", types.MaxSandboxCPUs))
	}
	memoryBytes, err := parseBytes(o.memory)
	if err != nil {
		return core.CreateSandboxRequest{}, invalidFlag("memory", err)
	}
	if memoryBytes < types.MinSandboxMemory {
		return core.CreateSandboxRequest{}, invalidFlag("memory", fmt.Errorf("must be at least %d bytes", types.MinSandboxMemory))
	}
	storageBytes, err := parseBytes(o.storageSize)
	if err != nil {
		return core.CreateSandboxRequest{}, invalidFlag("storage", err)
	}
	if storageBytes < types.MinSandboxStorage {
		return core.CreateSandboxRequest{}, invalidFlag("storage", fmt.Errorf("must be at least %d bytes", types.MinSandboxStorage))
	}
	if o.nics < 0 || o.nics > types.MaxSandboxNICs {
		return core.CreateSandboxRequest{}, invalidFlag("nics", fmt.Errorf("must be between 0 and %d", types.MaxSandboxNICs))
	}
	if o.nics == 0 && o.networkName != "" {
		return core.CreateSandboxRequest{}, invalidFlag("network", errors.New("requires at least one NIC"))
	}
	sandboxConfig := types.SandboxConfig{
		Name: o.name, CPUs: o.cpus, Memory: memoryBytes, SharedMemory: o.sharedMemory, Storage: storageBytes,
		NICs: o.nics, NetworkName: o.networkName,
		CPUPolicy: o.cpuPolicy, HugePages: o.hugePages, Mergeable: o.mergeable, NoBalloon: o.noBalloon,
	}
	requested := make([]types.DataDiskSpec, 0, len(o.dataDisks))
	for _, value := range o.dataDisks {
		disk, err := parseDataDisk(value)
		if err != nil {
			return core.CreateSandboxRequest{}, invalidFlag("data-disk", err)
		}
		requested = append(requested, disk)
	}
	sandboxConfig.DataDisks, err = types.ResolveDataDisks(nil, requested)
	if err != nil {
		return core.CreateSandboxRequest{}, invalidFlag("data-disk", err)
	}
	if err := sandboxConfig.Validate(); err != nil {
		return core.CreateSandboxRequest{}, err
	}
	if o.backend != "" {
		if err := types.VMMType(o.backend).Validate(); err != nil {
			return core.CreateSandboxRequest{}, invalidFlag("vmm", err)
		}
	}
	return core.CreateSandboxRequest{ImageReference: imageReference, Config: sandboxConfig, VMM: types.VMMType(o.backend)}, nil
}

// parseDataDisk accepts a compact, explicit disk specification without
// allowing unrecognized keys to silently change the requested disk shape.
func parseDataDisk(value string) (types.DataDiskSpec, error) {
	spec := types.DataDiskSpec{FSType: "ext4"}
	seen := make(map[string]bool)
	for part := range strings.SplitSeq(value, ",") {
		key, raw, ok := strings.Cut(part, "=")
		key, raw = strings.TrimSpace(key), strings.TrimSpace(raw)
		if !ok || raw == "" || seen[key] {
			return spec, fmt.Errorf("invalid or duplicate data disk field %q", part)
		}
		seen[key] = true
		switch key {
		case "name":
			spec.Name = raw
		case "size":
			size, err := parseBytes(raw)
			if err != nil {
				return spec, err
			}
			spec.Size = size
		case "fstype":
			spec.FSType = raw
		case "directio":
			switch raw {
			case "on":
				enabled := true
				spec.DirectIO = &enabled
			case "off":
				enabled := false
				spec.DirectIO = &enabled
			case "auto":
				spec.DirectIO = nil
			default:
				return spec, fmt.Errorf("directio must be on, off, or auto")
			}
		default:
			return spec, fmt.Errorf("unknown data disk field %q", key)
		}
	}
	if spec.Size < types.MinDataDiskSize || spec.FSType != "ext4" && spec.FSType != "none" {
		return spec, fmt.Errorf("data disk size must be at least %d bytes and fstype must be ext4 or none", types.MinDataDiskSize)
	}
	if spec.Name != "" {
		return spec, spec.Validate()
	}
	return spec, nil
}

// NewCreateCommand builds the top-level create command.
func NewCreateCommand(configuration configProvider) *cobra.Command {
	options := defaultCreateOptions()
	asJSON := false
	command := &cobra.Command{
		Use:   "create IMAGE",
		Short: "create a sandbox without starting it",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			request, err := options.request(args[0])
			if err != nil {
				return err
			}
			progress, err := startCreateProgress(command, options.name)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			service, err := core.OpenSandbox(command.Context(), configuration(), progress)
			if err != nil {
				return err
			}
			committed := false
			defer func() {
				closeErr := service.Close()
				returnErr = errors.Join(returnErr, errdefs.WithContext(closeErr, errdefs.ContextInfo{
					Operation: "create sandbox",
					Entity:    options.name,
					Phase:     "close metadata",
					Action:    "inspect the sandbox before retrying",
					Committed: committed,
				}))
			}()
			record, err := service.Create(command.Context(), request)
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.WithContext(err, errdefs.ContextInfo{
					Operation: "create sandbox",
					Entity:    options.name,
					Phase:     "output",
					Action:    "sandbox was created; inspect it before retrying",
					Committed: true,
				})
			}
			return nil
		},
	}
	options.addFlags(command)
	command.Flags().BoolVar(&asJSON, "json", false, "print the created sandbox as indented JSON")
	return command
}

// parseBytes accepts integer bytes or binary IEC units without floating-point rounding.
func parseBytes(value string) (int64, error) {
	return types.ParseByteSize(value)
}

// invalidFlag attaches user-correctable classification to size parsing errors.
func invalidFlag(name string, cause error) error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("--%s: %w", name, cause))
}
