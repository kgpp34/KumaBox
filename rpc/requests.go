package rpc

import "github.com/kumabox/kumabox/types"

// Ref addresses one exact runtime resource by name or ID.
type Ref struct{ Reference string }

// Target applies operation-specific options to a referenced resource.
type Target[T any] struct {
	Reference string
	Options   T
}

// Named selects a named child resource such as a disk or background process.
type Named struct {
	Reference string
	Name      string
}

// FileRequest addresses guest paths and explicit mutation options.
type FileRequest struct {
	Reference   string
	Path        string
	Destination string
	Parents     bool
	Recursive   bool
	Options     types.FileWriteOptions
}

// ProcessOutput selects a background process and its resumable output cursor.
type ProcessOutput struct {
	Reference string
	ID        string
	Options   types.ProcessOutputOptions
}

// ProcessSignal targets an agent-owned process, never a host PID.
type ProcessSignal struct {
	Reference string
	ID        string
	Signal    types.ProcessSignal
}

// PCIRequest selects one host device and its requested guest identifier.
type PCIRequest struct {
	Reference string
	PCI       string
	ID        string
}

// RestoreRequest identifies both destination sandbox and source snapshot.
type RestoreRequest struct {
	Reference string
	Snapshot  string
	Options   types.RestoreOptions
}

// SnapshotImport labels an incoming archive stream.
type SnapshotImport struct {
	Name        string
	Description string
}

// ImageImport selects an operator-local source or a registry reference.
type ImageImport struct {
	Name      string
	Path      string
	Format    string
	SourceTag string
	Platform  types.Platform
	Registry  string
}

// LayerProgress is an out-of-order image layer completion event.
type LayerProgress struct {
	Position int
	Total    int
	Digest   types.Digest
}

// ConsoleInput transports terminal bytes and resize notifications together.
type ConsoleInput struct {
	Data    []byte `json:"data,omitempty"`
	Rows    uint16 `json:"rows,omitempty"`
	Columns uint16 `json:"columns,omitempty"`
}
