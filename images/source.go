package images

import (
	"context"
	"io"
)

type Source interface {
	Resolve(context.Context, Platform) (Manifest, error)
	OpenLayer(context.Context, Descriptor) (io.ReadCloser, error)
}

type Converter interface {
	Convert(context.Context, Descriptor, io.Reader, string) (ConvertedLayer, error)
}

type ConvertedLayer struct {
	SourceDigest Digest
	EROFSPath    string
	EROFSDigest  Digest
	Size         int64
	BootFiles    []StagedBootFile
	Whiteouts    []string
	BootOpaque   bool
}

type StagedBootFile struct {
	Name string
	Path string
}

type Reporter interface {
	Layer(int, int, Digest) error
	Committed(Image) error
}

type DiscardReporter struct{}

func (DiscardReporter) Layer(int, int, Digest) error { return nil }
func (DiscardReporter) Committed(Image) error        { return nil }

// Limits bound compressed input and decompressed source and boot artifacts.
type Limits struct {
	LayerSize    int64
	UnpackedSize int64
	BootSize     int64
	ArchiveSize  int64
}

func DefaultLimits() Limits {
	return Limits{LayerSize: 8 << 30, UnpackedSize: 16 << 30, BootSize: 512 << 20, ArchiveSize: 32 << 30}
}

func (l Limits) Valid() bool {
	return l.LayerSize > 0 && l.UnpackedSize > 0 && l.BootSize > 0 && l.ArchiveSize > 0 && l.LayerSize < 1<<63-1 && l.UnpackedSize < 1<<63-1 && l.BootSize < 1<<63-1 && l.ArchiveSize < 1<<63-1
}
