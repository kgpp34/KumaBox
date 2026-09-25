package source

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
	"github.com/kumabox/kumabox/types"
)

// NewRegistry parses a registry reference and returns its normalized storage name.
// Resolve performs platform-specific requests using the default credential
// keychain; blob reads remain lazy. User-facing errors omit transport diagnostics
// that may contain credentials, while Unwrap preserves their causes.
func NewRegistry(reference string) (images.Source, string, error) {
	// URL userinfo must never reach parser diagnostics or stored image names.
	if strings.Contains(reference, "://") {
		return nil, "", invalidSource("use an OCI reference without a URL scheme or credentials")
	}
	parsed, err := name.ParseReference(reference)
	if err != nil {
		return nil, "", errdefs.New(errdefs.ClassInvalid, errdefs.CodeInvalidArgument, &safeRegistryError{cause: err, message: "invalid OCI registry reference"})
	}
	source := &resolvedSource{limits: images.DefaultLimits()}
	source.resolve = func(ctx context.Context, platform types.Platform) (v1.Image, error) {
		image, err := remote.Image(parsed, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithPlatform(v1.Platform{OS: platform.OS, Architecture: platform.Architecture}))
		if err != nil {
			return nil, registryError(err)
		}
		return &registryImage{Image: image}, nil
	}
	return source, parsed.String(), nil
}

// safeRegistryError separates a safe display message from diagnostic error identity.
type safeRegistryError struct {
	// cause remains accessible to errors.Is and errors.As.
	cause error
	// message is controlled locally rather than copied from transport.
	message string
}

// Error exposes only the locally supplied, credential-safe message.
func (e *safeRegistryError) Error() string { return e.message }

// Unwrap retains the underlying cause without displaying its text.
func (e *safeRegistryError) Unwrap() error { return e.cause }

// registryError preserves cancellation, maps HTTP 404 to not found, and wraps
// other failures with a safe availability message.
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

// registryImage sanitizes lazy metadata and layer lookup failures because registry
// I/O continues after remote.Image returns.
type registryImage struct {
	// Image retains library behavior for operations not overridden here.
	v1.Image
}

// RawManifest reads manifest bytes with sanitized registry errors.
func (i *registryImage) RawManifest() ([]byte, error) {
	b, e := i.Image.RawManifest()
	return b, registryError(e)
}

// RawConfigFile reads config bytes with sanitized registry errors.
func (i *registryImage) RawConfigFile() ([]byte, error) {
	b, e := i.Image.RawConfigFile()
	return b, registryError(e)
}

// LayerByDigest wraps lazy layer reads as well as lookup failures.
func (i *registryImage) LayerByDigest(h v1.Hash) (v1.Layer, error) {
	layer, err := i.Image.LayerByDigest(h)
	if err != nil {
		return nil, registryError(err)
	}
	return &registryLayer{Layer: layer}, nil
}

// registryLayer extends safe error presentation to encoded layer downloads.
type registryLayer struct {
	// Layer supplies the underlying registry-backed layer operations.
	v1.Layer
}

// Compressed wraps stream reads and cleanup, not just the opening request.
func (l *registryLayer) Compressed() (io.ReadCloser, error) {
	reader, err := l.Layer.Compressed()
	if err != nil {
		return nil, registryError(err)
	}
	return &registryReader{ReadCloser: reader}, nil
}

// registryReader sanitizes failures that occur after an HTTP response is opened.
type registryReader struct {
	// ReadCloser owns the original registry response stream.
	io.ReadCloser
}

// Read preserves EOF so streaming digest checks can finish normally.
func (r *registryReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		return n, err
	}
	return n, registryError(err)
}

// Close sanitizes transport failures while releasing the response stream.
func (r *registryReader) Close() error { return registryError(r.ReadCloser.Close()) }
