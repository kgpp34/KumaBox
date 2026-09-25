// Package network defines the host network provider contract used by the
// sandbox service. Concrete CNI and bridge implementations live in child
// packages and provider-private cleanup state never crosses this boundary.
package network

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kumabox/kumabox/types"
)

const (
	// DefaultQueueSize is the descriptor count used by supported VMM backends.
	DefaultQueueSize = 512
	// linuxInterfaceNameLimit excludes the terminating NUL byte.
	linuxInterfaceNameLimit = 15
)

// ErrNotConfigured reports that no usable infrastructure network definition
// is installed on the host.
var ErrNotConfigured = errors.New("network provider is not configured")

// AddSpec describes one NIC allocation. Existing is set during host recovery
// so the provider can preserve the durable MAC and IP identity.
type AddSpec struct {
	// Index is the stable zero-based NIC position.
	Index int
	// Queues overrides the CPU-derived queue count when positive.
	Queues int
	// Existing carries the identity that recovery must preserve.
	Existing *types.NetworkInterface
}

// Provider owns host network namespaces, CNI allocations, and TAP plumbing for
// a sandbox. Callers serialize operations for one sandbox identifier.
type Provider interface {
	// Type returns the durable backend identity.
	Type() types.NetworkBackend
	// Prepare creates or recovers the sandbox network namespace.
	Prepare(context.Context, types.SandboxID) (string, error)
	// Add allocates and wires the requested interfaces.
	Add(context.Context, types.SandboxID, string, ...AddSpec) ([]types.NetworkInterface, error)
	// Verify proves that the namespace and expected TAP devices are present.
	Verify(context.Context, types.SandboxID, []types.NetworkInterface) error
	// Recover reconstructs missing host state while preserving guest identity.
	Recover(context.Context, types.SandboxID, string, []types.NetworkInterface) ([]types.NetworkInterface, error)
	// Quiesce disables CNI-side links while a VMM is stopped.
	Quiesce(context.Context, types.SandboxID) error
	// Unquiesce restores links immediately before a VMM launch.
	Unquiesce(context.Context, types.SandboxID) error
	// Delete releases every allocation and the private namespace. It is
	// retryable after partial failure.
	Delete(context.Context, types.SandboxID) error
}

// AddRange builds fresh NIC requests for a contiguous index range.
func AddRange(first, count int) []AddSpec {
	if first < 0 || count <= 0 {
		return nil
	}
	result := make([]AddSpec, count)
	for offset := range result {
		result[offset] = AddSpec{Index: first + offset}
	}
	return result
}

// QueueCount returns two virtio queues per vCPU with a minimum RX/TX pair.
func QueueCount(cpus uint32) int { return max(2, int(cpus)*2) }

// ResolveQueues returns an explicit valid queue count or the CPU-derived
// default. Invalid explicit values are rejected by the provider.
func ResolveQueues(requested int, cpus uint32) int {
	if requested > 0 {
		return requested
	}
	return QueueCount(cpus)
}

// TAPName derives a deterministic Linux interface name within IFNAMSIZ. The
// UUID prefix plus NIC index remains unique within a sandbox namespace.
func TAPName(prefix string, id types.SandboxID, index int) (string, error) {
	if prefix == "" || index < 0 || strings.ContainsAny(prefix, "/\x00") {
		return "", errors.New("TAP prefix and NIC index are invalid")
	}
	for _, character := range prefix {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' {
			return "", errors.New("TAP prefix and NIC index are invalid")
		}
	}
	suffix := "-" + strconv.Itoa(index)
	compact := strings.ReplaceAll(id.String(), "-", "")
	const identityLength = 8
	if len(prefix)+identityLength+len(suffix) > linuxInterfaceNameLimit || len(compact) < identityLength {
		return "", fmt.Errorf("TAP prefix %q and NIC index %d exceed Linux name limits", prefix, index)
	}
	return prefix + compact[:identityLength] + suffix, nil
}
