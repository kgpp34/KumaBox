// Package sandbox exposes sandbox lifecycle commands through Cobra.
// It owns argument parsing and terminal presentation while core owns application
// ordering and assembles concrete adapters.
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
)

// rootsProvider reads persistent flags only after Cobra has parsed them.
type rootsProvider func() storage.Roots

// NewCreateCommand builds the top-level create command.
func NewCreateCommand(roots rootsProvider) *cobra.Command {
	name := ""
	cpus := types.DefaultSandboxCPUs
	memory := "1GiB"
	storageSize := "10GiB"
	asJSON := false
	command := &cobra.Command{
		Use:   "create IMAGE",
		Short: "create a sandbox without starting it",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (returnErr error) {
			memoryBytes, err := parseBytes(memory)
			if err != nil {
				return invalidFlag("memory", err)
			}
			storageBytes, err := parseBytes(storageSize)
			if err != nil {
				return invalidFlag("storage", err)
			}
			config := types.SandboxConfig{Name: name, CPUs: cpus, Memory: memoryBytes, Storage: storageBytes}
			if err := config.Validate(); err != nil {
				return err
			}
			progress, err := startCreateProgress(command, name)
			if err != nil {
				return err
			}
			defer func() { returnErr = errors.Join(returnErr, progress.Finish(returnErr)) }()
			service, err := core.OpenSandbox(command.Context(), roots(), progress)
			if err != nil {
				return err
			}
			committed := false
			defer func() {
				closeErr := service.Close()
				returnErr = errors.Join(returnErr, errdefs.Context(closeErr, "create sandbox", name, "close metadata", "inspect the sandbox before retrying", committed))
			}()
			record, err := service.Create(command.Context(), core.CreateSandboxRequest{ImageReference: args[0], Config: config})
			if err != nil {
				return err
			}
			committed = true
			if err := writeResult(progress.Output(command.OutOrStdout()), record, asJSON); err != nil {
				return errdefs.Context(err, "create sandbox", name, "output", "sandbox was created; inspect it before retrying", true)
			}
			return nil
		},
	}
	command.Flags().StringVar(&name, "name", name, "required sandbox name")
	command.Flags().Uint32Var(&cpus, "cpus", cpus, "number of virtual CPUs")
	command.Flags().StringVar(&memory, "memory", memory, "guest memory (for example 1GiB)")
	command.Flags().StringVar(&storageSize, "storage", storageSize, "logical sparse COW size (minimum 10GiB)")
	command.Flags().BoolVar(&asJSON, "json", false, "print the created sandbox as indented JSON")
	return command
}

// result is the stable JSON projection returned by create --json.
type result struct {
	// ID is the complete immutable sandbox UUID.
	ID string `json:"id"`
	// Name is the human-readable lookup key supplied by the user.
	Name string `json:"name"`
	// ImageDigest is the exact pinned manifest identity.
	ImageDigest string `json:"image_digest"`
	// State is created after persistent resources are ready.
	State string `json:"state"`
	// CPUs is the requested virtual CPU count.
	CPUs uint32 `json:"cpus"`
	// Memory is requested guest memory in bytes.
	Memory int64 `json:"memory"`
	// Storage is the logical sparse COW size in bytes.
	Storage int64 `json:"storage"`
	// Generation fences stale lifecycle transitions.
	Generation uint64 `json:"generation"`
	// CreatedAt is the identity reservation time.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the Created transition time.
	UpdatedAt time.Time `json:"updated_at"`
}

// writeResult keeps the default output script-friendly and JSON complete.
func writeResult(writer io.Writer, sandbox types.Sandbox, asJSON bool) error {
	if !asJSON {
		_, err := fmt.Fprintln(writer, sandbox.ID)
		return err
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result{
		ID: sandbox.ID.String(), Name: sandbox.Config.Name, ImageDigest: sandbox.ImageDigest.String(),
		State: string(sandbox.State), CPUs: sandbox.Config.CPUs, Memory: sandbox.Config.Memory,
		Storage: sandbox.Config.Storage, Generation: sandbox.Generation,
		CreatedAt: sandbox.CreatedAt, UpdatedAt: sandbox.UpdatedAt,
	})
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
