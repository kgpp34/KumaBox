package network

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

// Registry routes durable network backend identities to provider adapters.
// Construction freezes the available set so lifecycle operations never rely
// on package initialization or registration order.
type Registry struct {
	providers map[types.NetworkBackend]Provider
}

// NewRegistry validates and freezes the supplied provider set.
func NewRegistry(providers ...Provider) (*Registry, error) {
	registered := make(map[types.NetworkBackend]Provider, len(providers))
	for _, provider := range providers {
		if provider == nil || isNilProvider(provider) {
			return nil, errors.New("network registry contains a nil provider")
		}
		backend := provider.Type()
		if err := backend.Validate(); err != nil {
			return nil, fmt.Errorf("register network provider: %w", err)
		}
		if _, exists := registered[backend]; exists {
			return nil, fmt.Errorf("network provider %q is registered more than once", backend)
		}
		registered[backend] = provider
	}
	return &Registry{providers: registered}, nil
}

func isNilProvider(provider Provider) bool {
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Provider returns the adapter for a persisted backend identity.
func (r *Registry) Provider(backend types.NetworkBackend) (Provider, error) {
	if err := backend.Validate(); err != nil {
		return nil, errdefs.New(errdefs.ClassCorrupt, errdefs.CodeArtifactCorrupt, err)
	}
	if r == nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, errors.New("network registry is not configured"))
	}
	provider, exists := r.providers[backend]
	if !exists || provider == nil {
		return nil, errdefs.New(errdefs.ClassInvalid, errdefs.CodeHostIncompatible, fmt.Errorf("network backend %q is not available", backend))
	}
	return provider, nil
}

// Len returns the number of providers frozen into the registry.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.providers)
}
