package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/kumabox/kumabox/core"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/rpc"
	"github.com/kumabox/kumabox/types"
)

func registerImages(mux *http.ServeMux, app *core.Application) {
	store := app.Images
	rpc.Register(mux, "image.list", func(ctx context.Context, _ *rpc.Session, _ struct{}) (any, error) { return store.Catalog.List(ctx) })
	rpc.Register(mux, "image.inspect", func(ctx context.Context, _ *rpc.Session, p rpc.Ref) (any, error) {
		return store.Catalog.Resolve(ctx, p.Reference)
	})
	rpc.Register(mux, "image.verify", func(ctx context.Context, _ *rpc.Session, p rpc.Ref) (any, error) {
		return images.Verify(ctx, store.Paths, store.Catalog, p.Reference)
	})
	rpc.Register(mux, "image.remove", func(ctx context.Context, _ *rpc.Session, p rpc.Ref) (any, error) {
		return images.Remove(ctx, store.Paths, store.Catalog, p.Reference)
	})
	rpc.Register(mux, "image.import", func(ctx context.Context, s *rpc.Session, p rpc.ImageImport) (result any, returnErr error) {
		var input images.Source
		name := p.Name
		if p.Registry != "" {
			var err error
			input, name, err = core.NewRegistrySource(p.Registry)
			if err != nil {
				return nil, err
			}
		} else {
			source, cleanup, err := store.OpenLocalSource(ctx, p.Path, core.LocalImageOptions{Format: p.Format, SourceTag: p.SourceTag})
			if err != nil {
				return nil, err
			}
			defer func() { returnErr = errors.Join(returnErr, cleanup()) }()
			input = source
		}
		importer, err := core.NewImageImporter(ctx, store, imageReporter{s}, p.Platform)
		if err != nil {
			return nil, err
		}
		return importer.Import(ctx, name, p.Platform, input)
	})
	rpc.Register(mux, "maintenance.collect", func(ctx context.Context, _ *rpc.Session, p types.SnapshotEvictionPolicy) (any, error) {
		return app.Maintenance.CollectWithPolicy(ctx, p)
	})
}
