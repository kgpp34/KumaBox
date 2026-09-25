// Package sandbox exposes sandbox lifecycle commands through Cobra.
// It owns argument parsing and terminal presentation while core owns application
// ordering and assembles concrete adapters.
package sandbox

import (
	"errors"
	"fmt"
	"math"
	"strconv"

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
	name        string
	cpus        uint32
	memory      string
	storageSize string
	nics        int
	networkName string
}

// defaultCreateOptions returns the public resource defaults for a new sandbox.
func defaultCreateOptions() createOptions {
	return createOptions{
		cpus: types.DefaultSandboxCPUs, memory: "1GiB", storageSize: "10GiB", nics: 1,
	}
}

// addFlags registers the resource shape accepted by sandbox creation commands.
func (o *createOptions) addFlags(command *cobra.Command) {
	command.Flags().StringVar(&o.name, "name", o.name, "required sandbox name")
	command.Flags().Uint32Var(&o.cpus, "cpus", o.cpus, "number of virtual CPUs")
	command.Flags().StringVar(&o.memory, "memory", o.memory, "guest memory (for example 1GiB)")
	command.Flags().StringVar(&o.storageSize, "storage", o.storageSize, "logical sparse COW size (minimum 10GiB)")
	command.Flags().IntVar(&o.nics, "nics", o.nics, "number of network interfaces (0 disables networking)")
	command.Flags().StringVar(&o.networkName, "network", o.networkName, "CNI network name (empty selects the default)")
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
		Name: o.name, CPUs: o.cpus, Memory: memoryBytes, Storage: storageBytes,
		NICs: o.nics, NetworkName: o.networkName,
	}
	if err := sandboxConfig.Validate(); err != nil {
		return core.CreateSandboxRequest{}, err
	}
	return core.CreateSandboxRequest{ImageReference: imageReference, Config: sandboxConfig}, nil
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
				returnErr = errors.Join(returnErr, errdefs.Context(closeErr, "create sandbox", options.name, "close metadata", "inspect the sandbox before retrying", committed))
			}()
			record, err := service.Create(command.Context(), request)
			if err != nil {
				return err
			}
			committed = true
			if err := writeSandboxResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.Context(err, "create sandbox", options.name, "output", "sandbox was created; inspect it before retrying", true)
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
	if value == "" {
		return 0, errors.New("size must not be empty")
	}
	digits := 0
	for digits < len(value) && value[digits] >= '0' && value[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	number, err := strconv.ParseInt(value[:digits], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", value, err)
	}
	multiplier, ok := map[string]int64{"": 1, "B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[value[digits:]]
	if !ok || number == 0 || number > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("invalid or overflowing size %q; use B, KiB, MiB, GiB, or TiB", value)
	}
	return number * multiplier, nil
}

// invalidFlag attaches user-correctable classification to size parsing errors.
func invalidFlag(name string, cause error) error {
	return errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, fmt.Errorf("--%s: %w", name, cause))
}
