package vmm

import (
	"context"
	"io"
	"testing"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

type registryBackend struct{ typ types.VMMType }

type nilRegistryBackend struct{ Backend }

func (*nilRegistryBackend) Type() types.VMMType { return types.VMMCloudHypervisor }

func (b registryBackend) Type() types.VMMType { return b.typ }
func (registryBackend) Preflight() error      { return nil }
func (registryBackend) Locate(context.Context, types.SandboxID, uint64) (Process, bool, error) {
	return Process{}, false, nil
}

func (registryBackend) Observe(context.Context, types.SandboxID, uint64) (Observation, error) {
	return Observation{}, nil
}
func (registryBackend) WaitReady(context.Context, Process) error            { return nil }
func (registryBackend) Launch(context.Context, LaunchPlan) (Process, error) { return Process{}, nil }
func (registryBackend) Abort(context.Context, Process) error                { return nil }
func (registryBackend) Stop(context.Context, Process) error                 { return nil }
func (registryBackend) Console(context.Context, Process) (io.ReadWriteCloser, error) {
	return nil, nil
}

func (registryBackend) DialVsock(context.Context, Process, uint32) (io.ReadWriteCloser, error) {
	return nil, nil
}

func (registryBackend) Logs(context.Context, types.SandboxID, LogOptions, io.Writer) error {
	return nil
}
func (registryBackend) Cleanup(context.Context, types.SandboxID) error    { return nil }
func (registryBackend) RemoveLogs(context.Context, types.SandboxID) error { return nil }

func TestRegistryRoutesAndRejectsInvalidSets(t *testing.T) {
	backend := registryBackend{typ: types.VMMCloudHypervisor}
	registry, err := NewRegistry(backend)
	if err != nil {
		t.Fatal(err)
	}
	got, err := registry.Backend(types.VMMCloudHypervisor)
	if err != nil || got != backend || registry.Len() != 1 {
		t.Fatalf("Backend() = %#v, %v; len = %d", got, err, registry.Len())
	}
	if _, err := NewRegistry(backend, backend); err == nil {
		t.Fatal("NewRegistry() accepted duplicate backend types")
	}
	if _, err := NewRegistry(registryBackend{}); err == nil {
		t.Fatal("NewRegistry() accepted an invalid backend type")
	}
	var nilBackend *nilRegistryBackend
	if _, err := NewRegistry(nilBackend); err == nil {
		t.Fatal("NewRegistry() accepted a typed nil backend")
	}
	other, err := NewRegistry(registryBackend{typ: types.VMMFirecracker})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Backend(types.VMMCloudHypervisor); err == nil {
		t.Fatal("independent registry leaked another instance's backend")
	}
	if got, err := registry.Backend(types.VMMCloudHypervisor); err != nil || got != backend {
		t.Fatalf("original registry changed after constructing another instance: %#v, %v", got, err)
	}
}

func TestRegistryClassifiesLookupFailures(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Backend(types.VMMCloudHypervisor); err == nil {
		t.Fatal("Backend() found an unregistered backend")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeHostIncompatible {
		t.Fatalf("missing backend error = %v", err)
	}
	if _, err := registry.Backend(types.VMMType("broken")); err == nil {
		t.Fatal("Backend() accepted an invalid persisted type")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeArtifactCorrupt {
		t.Fatalf("invalid type error = %v", err)
	}
}
