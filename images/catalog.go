package images

import "context"

type CatalogReader interface {
	Resolve(context.Context, string) (Image, error)
	List(context.Context) ([]Image, error)
	FindLayers(context.Context, []Digest) (map[Digest]Layer, error)
}

type CatalogWriter interface {
	CommitImport(context.Context, ImportCommit) error
	Remove(context.Context, string, Digest) (Removal, error)
}

type Catalog interface {
	CatalogReader
	CatalogWriter
}
