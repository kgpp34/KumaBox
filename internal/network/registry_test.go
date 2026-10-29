package network

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kumabox/kumabox/internal/errdefs"
	"github.com/kumabox/kumabox/internal/types"
)

type closingProvider struct {
	registryProvider
	closes atomic.Int32
	err    error
}

func (p *closingProvider) Close() error {
	p.closes.Add(1)
	return p.err
}

func TestRegistryCloseReleasesEveryProviderOnce(t *testing.T) {
	failure := errors.New("descriptor close failed")
	first := &closingProvider{registryProvider: registryProvider{backend: types.NetworkBackendEBPF}, err: failure}
	second := &closingProvider{registryProvider: registryProvider{backend: types.NetworkBackendBridge}}
	registry, err := NewRegistry(first, second, &registryProvider{backend: types.NetworkBackendCNI})
	if err != nil {
		t.Fatal(err)
	}
	var callers sync.WaitGroup
	for range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if err := registry.Close(); !errors.Is(err, failure) {
				t.Errorf("close failure = %v", err)
			}
		}()
	}
	callers.Wait()
	if first.closes.Load() != 1 || second.closes.Load() != 1 {
		t.Fatalf("provider closes = %d, %d", first.closes.Load(), second.closes.Load())
	}
}

type registryProvider struct{ backend types.NetworkBackend }

func (p *registryProvider) Type() types.NetworkBackend { return p.backend }
func (*registryProvider) Prepare(context.Context, types.SandboxID) (string, error) {
	return "", nil
}

func (*registryProvider) Add(context.Context, types.SandboxID, string, ...AddSpec) ([]types.NetworkInterface, error) {
	return nil, nil
}

func (*registryProvider) Verify(context.Context, types.SandboxID, []types.NetworkInterface) error {
	return nil
}

func (*registryProvider) Recover(context.Context, types.SandboxID, string, []types.NetworkInterface) ([]types.NetworkInterface, error) {
	return nil, nil
}
func (*registryProvider) Quiesce(context.Context, types.SandboxID) error   { return nil }
func (*registryProvider) Unquiesce(context.Context, types.SandboxID) error { return nil }
func (*registryProvider) Delete(context.Context, types.SandboxID) error    { return nil }

func TestRegistryRoutesPersistedBackend(t *testing.T) {
	provider := &registryProvider{backend: types.NetworkBackendCNI}
	registry, err := NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Provider(types.NetworkBackendCNI)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != provider || registry.Len() != 1 {
		t.Fatalf("resolved provider = %T, len = %d", resolved, registry.Len())
	}
}

func TestRegistryRejectsInvalidSetsAndUnavailableBackends(t *testing.T) {
	var typedNil *registryProvider
	if _, err := NewRegistry(typedNil); err == nil {
		t.Fatal("NewRegistry accepted a typed nil provider")
	}
	provider := &registryProvider{backend: types.NetworkBackendCNI}
	if _, err := NewRegistry(provider, provider); err == nil {
		t.Fatal("NewRegistry accepted a duplicate provider")
	}
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Provider(types.NetworkBackendCNI); err == nil {
		t.Fatal("Provider resolved an unavailable backend")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeHostIncompatible {
		t.Fatalf("Provider error code = %q, %v; want %q", code, err, errdefs.CodeHostIncompatible)
	}
}
