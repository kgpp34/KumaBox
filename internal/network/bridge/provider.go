// Package bridge attaches sandbox TAP devices directly to a pre-existing host
// Linux bridge. It deliberately uses no CNI plugin, namespace, veth or TC
// redirect. Guests use DHCP or the provider's persistent static address leases.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/types"
)

// Options contains immutable policy for one bridge provider.
type Options struct {
	// TAPPrefix begins every managed TAP name. It must fit the Linux IFNAMSIZ
	// budget together with the sandbox identity and NIC index.
	TAPPrefix string
	// QueueSize is the virtio queue descriptor count reported to the VMM.
	QueueSize int
	// LeaseDir stores one small allocation record per bridge IP. It must be
	// on persistent storage so restart/recovery keeps guest addresses stable.
	LeaseDir string
}

// Validate rejects ambiguous provider configuration before it is registered.
func (o Options) Validate() error {
	if o.TAPPrefix == "" {
		return errors.New("bridge TAP prefix must not be empty")
	}
	if len(o.TAPPrefix) > 5 {
		return errors.New("bridge TAP prefix is too long for Linux interface names")
	}
	for _, character := range o.TAPPrefix {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' {
			return fmt.Errorf("bridge TAP prefix %q contains an invalid character", o.TAPPrefix)
		}
	}
	if o.QueueSize <= 0 {
		return errors.New("bridge queue size must be positive")
	}
	if o.LeaseDir == "" || !filepath.IsAbs(o.LeaseDir) {
		return errors.New("bridge lease directory must be absolute")
	}
	return nil
}

// Provider owns direct-bridge TAP lifecycle for one KumaBox installation.
type Provider struct {
	prefix    string
	queueSize int
	leaseDir  string
}

// Type returns the durable backend identity.
func (*Provider) Type() types.NetworkBackend { return types.NetworkBackendBridge }

// Prepare is a no-op: bridge TAPs live in the host network namespace.
func (*Provider) Prepare(context.Context, types.SandboxID) (string, error) { return "", nil }

// Quiesce is a no-op: a bridge TAP has no CNI-side veth to hold down.
func (*Provider) Quiesce(context.Context, types.SandboxID) error { return nil }

// Unquiesce is a no-op: the TAP stays attached to the bridge across stop/start.
func (*Provider) Unquiesce(context.Context, types.SandboxID) error { return nil }

var _ network.Provider = (*Provider)(nil)
