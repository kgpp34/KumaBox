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
	// NICs is the requested network interface count.
	NICs int `json:"nics"`
	// NetworkName is the resolved CNI network name.
	NetworkName string `json:"network_name,omitempty"`
	// Network is the resolved provider-to-VMM handoff.
	Network *networkOutput `json:"network,omitempty"`
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

// networkOutput is the stable JSON projection of resolved sandbox networking.
type networkOutput struct {
	// Backend identifies the provider that owns host network resources.
	Backend string `json:"backend"`
	// Namespace is the absolute network namespace path.
	Namespace string `json:"namespace"`
	// Interfaces lists NICs in stable guest index order.
	Interfaces []networkInterfaceOutput `json:"interfaces"`
}

// networkInterfaceOutput describes one guest NIC and its host TAP endpoint.
type networkInterfaceOutput struct {
	// Index is the stable zero-based guest NIC position.
	Index int `json:"index"`
	// Name is the guest interface name.
	Name string `json:"name"`
	// TAP is the host-side device opened by the VMM.
	TAP string `json:"tap"`
	// MAC is the durable guest hardware address.
	MAC string `json:"mac"`
	// Queues is the total virtio RX and TX queue count.
	Queues int `json:"queues"`
	// QueueSize is the descriptor count for each queue.
	QueueSize int `json:"queue_size"`
	// Network is the resolved CNI conflist name.
	Network string `json:"network"`
	// IPv4 is the optional guest-visible IPv4 assignment.
	IPv4 *ipv4Output `json:"ipv4,omitempty"`
}

// ipv4Output is the stable JSON projection of a guest IPv4 assignment.
type ipv4Output struct {
	// Address is the guest IPv4 address without a prefix.
	Address string `json:"address"`
	// Gateway is the optional default gateway.
	Gateway string `json:"gateway,omitempty"`
	// Prefix is the CIDR prefix length.
	Prefix int `json:"prefix"`
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
		Storage: sandbox.Config.Storage, NICs: sandbox.Config.NICs, NetworkName: sandbox.Config.NetworkName,
		Generation: sandbox.Generation,
		CreatedAt:  sandbox.CreatedAt.UTC(), UpdatedAt: sandbox.UpdatedAt.UTC(),
	}
	if sandbox.Network.Backend != "" {
		result.Network = networkResult(sandbox.Network)
	}
	if sandbox.Failure != nil {
		result.Failure = &sandboxFailureOutput{Phase: sandbox.Failure.Phase, Message: sandbox.Failure.Message}
	}
	return result
}

func networkResult(setup types.NetworkSetup) *networkOutput {
	result := &networkOutput{
		Backend: string(setup.Backend), Namespace: setup.Namespace,
		Interfaces: make([]networkInterfaceOutput, 0, len(setup.Interfaces)),
	}
	for _, networkInterface := range setup.Interfaces {
		item := networkInterfaceOutput{
			Index: networkInterface.Index, Name: networkInterface.Name, TAP: networkInterface.TAP,
			MAC: networkInterface.MAC, Queues: networkInterface.Queues, QueueSize: networkInterface.QueueSize,
			Network: networkInterface.Network,
		}
		if networkInterface.IPv4 != nil {
			item.IPv4 = &ipv4Output{
				Address: networkInterface.IPv4.Address,
				Gateway: networkInterface.IPv4.Gateway,
				Prefix:  networkInterface.IPv4.Prefix,
			}
		}
		result.Interfaces = append(result.Interfaces, item)
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
	if _, err := fmt.Fprintln(table, "SANDBOX ID\tNAME\tIMAGE ID\tVMM\tSTATE\tCPUS\tMEMORY\tSTORAGE\tNICS\tNETWORK\tCREATED"); err != nil {
		return err
	}
	for _, record := range records {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%d\t%s\t%s\n",
			record.ID, record.Config.Name, record.ImageDigest.Hex()[:12], record.VMM, record.State, record.Config.CPUs,
			formatIECBytes(record.Config.Memory), formatIECBytes(record.Config.Storage), record.Config.NICs,
			record.Config.NetworkName,
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
