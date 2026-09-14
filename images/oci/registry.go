package oci

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
)

func NewRegistry(reference string) (images.Source, string, error) {
	// URL userinfo must never reach parser diagnostics or stored image names.
	if strings.Contains(reference, "://") {
		return nil, "", invalidSource("use an OCI reference without a URL scheme or credentials")
	}
	parsed, err := name.ParseReference(reference)
	if err != nil {
		return nil, "", errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, &safeRegistryError{cause: err, message: "invalid OCI registry reference"})
	}
	source := &resolvedSource{limits: DefaultLimits()}
	source.resolve = func(ctx context.Context, platform images.Platform) (v1.Image, error) {
		image, err := remote.Image(parsed, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithPlatform(v1.Platform{OS: platform.OS, Architecture: platform.Architecture}))
		if err != nil {
			return nil, registryError(err)
		}
		return &registryImage{Image: image}, nil
	}
	return source, parsed.String(), nil
}

type safeRegistryError struct {
	cause   error
	message string
}

func (e *safeRegistryError) Error() string { return e.message }
func (e *safeRegistryError) Unwrap() error { return e.cause }
func registryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var transportErr *transport.Error
	if errors.As(err, &transportErr) && transportErr.StatusCode == 404 {
		return errdefs.New(errdefs.ClassNotFound, errdefs.CodeNotFound, &safeRegistryError{cause: err, message: "registry image or blob not found"})
	}
	return errdefs.New(errdefs.ClassUnavailable, errdefs.CodeArtifactUnavailable, &safeRegistryError{cause: err, message: "registry request failed; check connectivity and credentials"})
}

// Wrap lazy operations too: registry I/O continues after remote.Image returns.
type registryImage struct{ v1.Image }

func (i *registryImage) RawManifest() ([]byte, error) {
	b, e := i.Image.RawManifest()
	return b, registryError(e)
}

func (i *registryImage) RawConfigFile() ([]byte, error) {
	b, e := i.Image.RawConfigFile()
	return b, registryError(e)
}

func (i *registryImage) LayerByDigest(h v1.Hash) (v1.Layer, error) {
	layer, err := i.Image.LayerByDigest(h)
	if err != nil {
		return nil, registryError(err)
	}
	return &registryLayer{Layer: layer}, nil
}

type registryLayer struct{ v1.Layer }

func (l *registryLayer) Compressed() (io.ReadCloser, error) {
	reader, err := l.Layer.Compressed()
	if err != nil {
		return nil, registryError(err)
	}
	return &registryReader{ReadCloser: reader}, nil
}

type registryReader struct{ io.ReadCloser }

func (r *registryReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		return n, err
	}
	return n, registryError(err)
}
func (r *registryReader) Close() error { return registryError(r.ReadCloser.Close()) }
