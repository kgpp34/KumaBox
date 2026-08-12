// Package protocol defines the stable wire vocabulary shared by the host and
// guest agent implementations.
package protocol

// RequestType identifies an agent request on the vsock stream.
type RequestType string

const (
	RequestPing     RequestType = "ping"
	RequestExec     RequestType = "exec"
	RequestIdentity RequestType = "identity"
	RequestReseed   RequestType = "reseed"
)

// Capability identifies an operation advertised by the guest agent.
type Capability string

const (
	CapabilityIdentity Capability = "identity"
	CapabilityReseed   Capability = "reseed"
)

const (
	CapabilityPingPong   Capability = "ping-pong"
	CapabilityExec       Capability = "exec"
	CapabilityExecStream Capability = "exec-stream"
	CapabilityExecTTY    Capability = "exec-tty"
)

// AgentPort is the vsock port used by the guest agent.
const AgentPort uint32 = 1024
