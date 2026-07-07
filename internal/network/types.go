// Package network manages host-side network intent for KumaBox VMs.
//
// The package separates VM render config from provider state. VM records keep a
// Config copy used by Cloud Hypervisor, while the network store keeps provider
// records, IP leases, and host-tap bridge ownership used for reconciliation and
// cleanup.
package network

import "time"

const (
	// ProviderHostTap is KumaBox's built-in Linux bridge + TAP provider.
	ProviderHostTap = "host-tap"

	// ProviderCNI is reserved for a future CNI-backed provider.
	ProviderCNI = "cni"

	// ProviderNone disables VM network attachment.
	ProviderNone = "none"
)

const maxInterfaceNameLength = 15

type AddSpec struct {
	Index    int
	Existing *Config
}

type Provider interface {
	Type() string
	List() ([]Record, error)
	Inspect(vmRef string) (*InspectResult, error)
}

// Config is the VM-side network attachment rendered into the VMM config.
//
// It is copied into VMRecord so a VM can be restarted with the same tap, MAC,
// and guest IP even if provider indexes need reconciliation.
type Config struct {
	ID        string     `json:"id,omitempty"`
	TAP       string     `json:"tap"`
	MAC       string     `json:"mac"`
	NumQueues int        `json:"numQueues"`
	QueueSize int        `json:"queueSize"`
	Backend   string     `json:"backend"`
	BridgeDev string     `json:"bridgeDev,omitempty"`
	NetnsPath string     `json:"netnsPath,omitempty"`
	Network   *GuestInfo `json:"network,omitempty"`
}

// GuestInfo is the static network configuration delivered to the guest.
//
// For cloud images this is rendered into cloud-init NoCloud network-config.
// Direct boot paths may use the same values through a later guest-agent flow.
type GuestInfo struct {
	IP      string   `json:"ip,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	Prefix  int      `json:"prefix,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

// Cleanup records a provider cleanup failure that needs retry or GC attention.
//
// A pending cleanup keeps the provider record in place rather than losing the
// tap/IP identity required to safely finish deletion later.
type Cleanup struct {
	Pending       bool   `json:"pending"`
	Reason        string `json:"reason,omitempty"`
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
}

// Record is the provider-side view of one VM network interface.
//
// Records are indexed outside the VM store so network commands can inspect and
// reconcile provider state independently from VM lifecycle state.
type Record struct {
	ID        string    `json:"id"`
	VMID      string    `json:"vmId"`
	Network   string    `json:"network"`
	Provider  string    `json:"provider"`
	IfName    string    `json:"ifName"`
	TAP       string    `json:"tap"`
	MAC       string    `json:"mac"`
	NumQueues int       `json:"numQueues"`
	QueueSize int       `json:"queueSize"`
	BridgeDev string    `json:"bridgeDev,omitempty"`
	NetnsPath string    `json:"netnsPath,omitempty"`
	IPs       []string  `json:"ips,omitempty"`
	Gateway   string    `json:"gateway,omitempty"`
	DNS       []string  `json:"dns,omitempty"`
	Cleanup   Cleanup   `json:"cleanup"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// InspectResult compares provider records with the VM's rendered network config.
//
// Drift is populated when either side is missing or important fields such as
// tap, MAC, backend, or guest IP disagree.
type InspectResult struct {
	VMID       string   `json:"vmId"`
	VMName     string   `json:"vmName,omitempty"`
	Network    string   `json:"network,omitempty"`
	Interfaces []Record `json:"interfaces"`
	VMConfigs  []Config `json:"vmConfigs,omitempty"`
	Drift      []string `json:"drift,omitempty"`
}

// HostTapState records ownership of the global host-tap bridge/NAT domain.
//
// RefCount tracks VM network attachments. VM delete decrements it; network
// teardown refuses to remove the bridge while references remain.
type HostTapState struct {
	SchemaVersion string    `json:"schemaVersion"`
	Bridge        string    `json:"bridge"`
	CIDR          string    `json:"cidr"`
	Gateway       string    `json:"gateway"`
	NATBackend    string    `json:"natBackend"`
	Owner         Owner     `json:"owner"`
	RefCount      int       `json:"refCount"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Owner identifies the KumaBox root that owns a host network resource.
//
// This prevents one root directory from tearing down a bridge created by
// another independent KumaBox state root.
type Owner struct {
	Kind    string `json:"kind"`
	RootDir string `json:"rootDir"`
}

// HostTapReport describes the changes made by setup or teardown.
type HostTapReport struct {
	Bridge     string        `json:"bridge"`
	CIDR       string        `json:"cidr"`
	Gateway    string        `json:"gateway"`
	NATBackend string        `json:"natBackend"`
	Created    bool          `json:"created"`
	Changed    []string      `json:"changed,omitempty"`
	State      *HostTapState `json:"state,omitempty"`
}
