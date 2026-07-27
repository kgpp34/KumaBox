// Package protocol defines the stable wire vocabulary shared by the host and
// guest agent implementations.
package protocol

// RequestType identifies an agent request on the vsock stream.
type RequestType string

const (
	RequestHello    RequestType = "hello"
	RequestExec     RequestType = "exec"
	RequestIdentity RequestType = "identity"
)

// Capability identifies an operation advertised by the guest agent.
type Capability string

const CapabilityIdentity Capability = "identity"

const (
	CapabilityHello      Capability = "hello"
	CapabilityExec       Capability = "exec"
	CapabilityExecStream Capability = "exec-stream"
)

// AgentPort is the vsock port used by the guest agent.
const AgentPort uint32 = 1024
