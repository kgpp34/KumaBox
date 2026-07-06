package network

import "time"

const (
	ProviderHostTap = "host-tap"
	ProviderCNI     = "cni"
	ProviderNone    = "none"
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

type GuestInfo struct {
	IP      string   `json:"ip,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	Prefix  int      `json:"prefix,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

type Cleanup struct {
	Pending       bool   `json:"pending"`
	Reason        string `json:"reason,omitempty"`
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
}

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

type InspectResult struct {
	VMID       string   `json:"vmId"`
	Interfaces []Record `json:"interfaces"`
	Drift      []string `json:"drift,omitempty"`
}
