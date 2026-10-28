package client

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/kumabox/kumabox/rpc"
	"github.com/kumabox/kumabox/types"
)

// ImageRemoval preserves aliases successfully removed by the server.
type ImageRemoval struct {
	Names  []string
	Layers []types.Digest
}

func (s *ImageService) List(ctx context.Context) ([]types.Image, error) {
	var result []types.Image
	err := s.connection.Call(ctx, "image.list", struct{}{}, &result, rpc.Streams{})
	return result, err
}

func (s *ImageService) Inspect(ctx context.Context, ref string) (types.Image, error) {
	var result types.Image
	err := s.connection.Call(ctx, "image.inspect", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

func (s *ImageService) Verify(ctx context.Context, ref string) (types.Image, error) {
	var result types.Image
	err := s.connection.Call(ctx, "image.verify", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

func (s *ImageService) Remove(ctx context.Context, ref string) (ImageRemoval, error) {
	var result ImageRemoval
	err := s.connection.Call(ctx, "image.remove", rpc.Ref{Reference: ref}, &result, rpc.Streams{})
	return result, err
}

// Import submits an absolute operator-local path to the local node service.
// The daemon stages and validates the source under its own configured limits.
func (s *ImageService) Import(ctx context.Context, name, path, format, tag string, platform types.Platform, reporter ImageReporter) (types.Image, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return types.Image{}, err
	}
	return s.importImage(ctx, rpc.ImageImport{Name: name, Path: absolute, Format: format, SourceTag: tag, Platform: platform}, reporter)
}

func (s *ImageService) Pull(ctx context.Context, reference string, platform types.Platform, reporter ImageReporter) (types.Image, error) {
	return s.importImage(ctx, rpc.ImageImport{Registry: reference, Platform: platform}, reporter)
}

func (s *ImageService) importImage(ctx context.Context, params rpc.ImageImport, reporter ImageReporter) (types.Image, error) {
	var result types.Image
	err := s.connection.Call(ctx, "image.import", params, &result, rpc.Streams{Event: func(frame rpc.Frame) error {
		if reporter == nil {
			return nil
		}
		switch frame.Type {
		case "layer":
			var layer rpc.LayerProgress
			if err := json.Unmarshal(frame.Data, &layer); err != nil {
				return err
			}
			return reporter.Layer(layer.Position, layer.Total, layer.Digest)
		case "image_committed":
			var image types.Image
			if err := json.Unmarshal(frame.Data, &image); err != nil {
				return err
			}
			return reporter.Committed(image)
		}
		return nil
	}})
	return result, err
}
