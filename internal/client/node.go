package client

import (
	"context"
	"io"

	"github.com/kumabox/kumabox/internal/rpc"
	"github.com/kumabox/kumabox/internal/types"
)

// Node submits node administration requests to an explicit Unix endpoint.
type Node struct{ connection *rpc.Client }

// New prepares a local node client; connections are established on first use.
func New(socket string) *Node { return &Node{connection: rpc.NewClient(socket)} }

// Close releases idle connections without changing running sandboxes.
func (n *Node) Close() error { return n.connection.Close() }

// Doctor runs the installed node checker and retains its process exit status.
func (n *Node) Doctor(ctx context.Context, args []string, out, stderr io.Writer) (int, error) {
	var code int
	err := n.connection.Call(ctx, "node.doctor", args, &code, rpc.Streams{Output: out, Error: stderr})
	return code, err
}

// Info reads the running node's build and authoritative ownership roots.
func (n *Node) Info(ctx context.Context) (types.NodeInfo, error) {
	var info types.NodeInfo
	err := n.connection.Call(ctx, "node.info", struct{}{}, &info, rpc.Streams{})
	return info, err
}
