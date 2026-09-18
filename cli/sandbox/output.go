package sandbox

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/kumabox/kumabox/types"
)

// sandboxOutput is the stable JSON projection shared by sandbox commands.
// It deliberately stays in the CLI package: types.Sandbox is the domain
// contract, while this flattened shape is a user-facing serialization contract.
type sandboxOutput struct {
	// ID is the complete immutable sandbox UUID.
	ID string `json:"id"`
	// Name is the exact human-readable lookup key.
	Name string `json:"name"`
	// ImageDigest is the exact pinned manifest identity.
	ImageDigest string `json:"image_digest"`
	// VMM is the backend that owns this sandbox's runtime.
	VMM string `json:"vmm"`
	// State is the durable sandbox lifecycle state.
	State string `json:"state"`
	// CPUs is the requested virtual CPU count.
	CPUs uint32 `json:"cpus"`
	// Memory is requested guest memory in bytes.
	Memory int64 `json:"memory"`
	// Storage is the logical sparse COW size in bytes.
	Storage int64 `json:"storage"`
	// Generation fences stale lifecycle transitions.
	Generation uint64 `json:"generation"`
	// Failure explains retained cleanup work for an error-state sandbox.
	Failure *sandboxFailureOutput `json:"failure,omitempty"`
	// CreatedAt is the identity reservation time.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the latest committed transition time.
	UpdatedAt time.Time `json:"updated_at"`
}

// sandboxFailureOutput is the user-facing diagnostic for an error-state sandbox.
type sandboxFailureOutput struct {
	// Phase identifies the lifecycle step that failed.
	Phase string `json:"phase"`
	// Message preserves the operator-facing failure detail.
	Message string `json:"message"`
}

// removeOutput is the stable JSON result for a completed sandbox removal.
type removeOutput struct {
	// ID is the immutable identity whose resources were deleted.
	ID string `json:"id"`
	// Name is the released user-facing sandbox name.
	Name string `json:"name"`
}

// sandboxResult projects a validated domain record into the CLI JSON schema.
func sandboxResult(sandbox types.Sandbox) sandboxOutput {
	result := sandboxOutput{
		ID: sandbox.ID.String(), Name: sandbox.Config.Name, ImageDigest: sandbox.ImageDigest.String(), VMM: string(sandbox.VMM),
		State: string(sandbox.State), CPUs: sandbox.Config.CPUs, Memory: sandbox.Config.Memory,
		Storage: sandbox.Config.Storage, Generation: sandbox.Generation,
		CreatedAt: sandbox.CreatedAt.UTC(), UpdatedAt: sandbox.UpdatedAt.UTC(),
	}
	if sandbox.Failure != nil {
		result.Failure = &sandboxFailureOutput{Phase: sandbox.Failure.Phase, Message: sandbox.Failure.Message}
	}
	return result
}

// writeSandboxJSON emits one complete sandbox as indented JSON.
func writeSandboxJSON(writer io.Writer, sandbox types.Sandbox) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(sandboxResult(sandbox))
}

// writeSandboxResult keeps lifecycle command output script-friendly and JSON complete.
func writeSandboxResult(writer io.Writer, sandbox types.Sandbox, asJSON bool) error {
	if !asJSON {
		_, err := fmt.Fprintln(writer, sandbox.ID)
		return err
	}
	return writeSandboxJSON(writer, sandbox)
}

// writeRemoveResult keeps text output script-friendly and JSON self-describing.
func writeRemoveResult(writer io.Writer, sandbox types.Sandbox, asJSON bool) error {
	if !asJSON {
		_, err := fmt.Fprintln(writer, sandbox.ID)
		return err
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(removeOutput{ID: sandbox.ID.String(), Name: sandbox.Config.Name})
}

// writeSandboxListJSON emits an array even when the metadata snapshot is empty.
func writeSandboxListJSON(writer io.Writer, records []types.Sandbox) error {
	results := make([]sandboxOutput, 0, len(records))
	for _, record := range records {
		results = append(results, sandboxResult(record))
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

// writeSandboxIDs emits complete IDs so every line can be passed directly to rm.
func writeSandboxIDs(writer io.Writer, records []types.Sandbox) error {
	for _, record := range records {
		if _, err := fmt.Fprintln(writer, record.ID); err != nil {
			return err
		}
	}
	return nil
}

// writeSandboxTable renders headers for empty results and keeps IDs actionable.
func writeSandboxTable(writer io.Writer, records []types.Sandbox) error {
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "SANDBOX ID\tNAME\tIMAGE ID\tVMM\tSTATE\tCPUS\tMEMORY\tSTORAGE\tCREATED"); err != nil {
		return err
	}
	for _, record := range records {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			record.ID, record.Config.Name, record.ImageDigest.Hex()[:12], record.VMM, record.State, record.Config.CPUs,
			formatIECBytes(record.Config.Memory), formatIECBytes(record.Config.Storage),
			record.CreatedAt.UTC().Format(time.RFC3339),
		); err != nil {
			return err
		}
	}
	return table.Flush()
}

// formatIECBytes renders binary resource sizes without losing their byte facts in JSON.
func formatIECBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%dB", size)
	}
	value := float64(size)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"} {
		value /= 1024
		if value < 1024 || unit == "EiB" {
			return fmt.Sprintf("%.1f%s", value, unit)
		}
	}
	return fmt.Sprintf("%dB", size)
}
