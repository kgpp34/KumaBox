package disk

import (
	"fmt"
	"os"
)

// ReflinkProbe describes whether the directory's filesystem supports local
// copy-on-write file cloning. The directory must be the actual destination
// directory used by the caller; probing another mount is not meaningful.
type ReflinkProbe struct {
	Directory string `json:"directory"`
	Supported bool   `json:"supported"`
}

// ProbeReflink tests FICLONE in directory without touching application data.
// Unsupported filesystems return Supported=false and a nil error so callers
// can choose the Cocoon-compatible sparse/stream fallback.
func ProbeReflink(directory string) (ReflinkProbe, error) {
	info, err := os.Stat(directory)
	if err != nil {
		return ReflinkProbe{}, fmt.Errorf("stat reflink directory: %w", err)
	}
	if !info.IsDir() {
		return ReflinkProbe{}, fmt.Errorf("reflink path is not a directory: %s", directory)
	}
	supported, err := probeReflink(directory)
	if err != nil {
		return ReflinkProbe{}, err
	}
	return ReflinkProbe{Directory: directory, Supported: supported}, nil
}
