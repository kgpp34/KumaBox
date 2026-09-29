package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

type resizeNetwork struct {
	*fakeNetwork
	owned     map[int]types.NetworkInterface
	removeErr error
}

func (f *resizeNetwork) Add(_ context.Context, id types.SandboxID, name string, specs ...network.AddSpec) ([]types.NetworkInterface, error) {
	*f.steps = append(*f.steps, "network-add")
	if name == "" {
		name = "bridge"
	}
	result := make([]types.NetworkInterface, 0, len(specs))
	for _, spec := range specs {
		tap, err := network.TAPName("tap", id, spec.Index)
		if err != nil {
			return nil, err
		}
		device := types.NetworkInterface{
			Index: spec.Index, Name: fmt.Sprintf("eth%d", spec.Index), TAP: tap,
			MAC: fmt.Sprintf("02:00:00:00:00:%02x", spec.Index+1), Queues: spec.Queues,
			QueueSize: network.DefaultQueueSize, Network: name,
		}
		f.owned[spec.Index] = device
		result = append(result, device)
	}
	return result, nil
}

func (f *resizeNetwork) Allocated(context.Context, types.SandboxID) ([]int, error) {
	indices := make([]int, 0, len(f.owned))
	for index := range f.owned {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	return indices, nil
}

func (*resizeNetwork) IndexForTAP(id types.SandboxID, tap string) (int, bool) {
	for index := range types.MaxSandboxNICs {
		expected, _ := network.TAPName("tap", id, index)
		if tap == expected {
			return index, true
		}
	}
	return 0, false
}

func (f *resizeNetwork) Remove(_ context.Context, _ types.SandboxID, indices ...int) error {
	*f.steps = append(*f.steps, "network-remove")
	if f.removeErr != nil {
		return f.removeErr
	}
	for _, index := range indices {
		delete(f.owned, index)
	}
	return nil
}

type resizeRuntime struct {
	*fakeRuntime
	live      map[string]vmm.NetworkDevice
	addErr    error
	failAddAt int
}

func (f *resizeRuntime) LiveNICs(context.Context, vmm.Process) ([]vmm.NetworkDevice, error) {
	result := make([]vmm.NetworkDevice, 0, len(f.live))
	for _, device := range f.live {
		result = append(result, device)
	}
	return result, nil
}

func (f *resizeRuntime) AddNIC(_ context.Context, _ vmm.Process, device types.NetworkInterface) error {
	*f.steps = append(*f.steps, "vmm-add")
	if f.addErr != nil && (f.failAddAt == 0 || f.failAddAt == device.Index) {
		return f.addErr
	}
	f.live[device.TAP] = vmm.NetworkDevice{ID: fmt.Sprintf("net-%d", device.Index), TAP: device.TAP, MAC: device.MAC}
	return nil
}

func (f *resizeRuntime) RemoveNIC(_ context.Context, _ vmm.Process, id string) error {
	*f.steps = append(*f.steps, "vmm-remove")
	for tap, device := range f.live {
		if device.ID == id {
			delete(f.live, tap)
		}
	}
	return nil
}

func newResizeService(t *testing.T, count int) (*SandboxService, *resizeNetwork, *resizeRuntime, *[]string) {
	t.Helper()
	service, steps := newTestSandboxService(t, nil)
	base := &fakeNetwork{steps: steps, namespace: "/var/run/netns/kb-test"}
	provider := &resizeNetwork{fakeNetwork: base, owned: make(map[int]types.NetworkInterface)}
	networks, err := network.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	service.dependencies.networks = networks
	underlying := &fakeRuntime{steps: steps, vsockFactory: guestTestConnection}
	process := vmm.Process{
		PID: 42, StartTicks: 10, BootID: "boot", SandboxID: fixedID, Generation: 3,
		Binary: "cloud-hypervisor", APISocket: "/run/kumabox/api.sock",
	}
	underlying.observation = vmm.Observation{State: vmm.ProcessRunning, Process: process}
	runtime := &resizeRuntime{fakeRuntime: underlying, live: make(map[string]vmm.NetworkDevice)}
	runtimes, err := vmm.NewRegistry(runtime)
	if err != nil {
		t.Fatal(err)
	}
	service.dependencies.runtimes = runtimes
	digest := service.dependencies.images.(fakeGuard).image.ManifestDigest
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	record := types.Sandbox{
		ID: fixedID, Config: types.SandboxConfig{
			Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage,
			NICs: count, NetworkName: "bridge",
		},
		ImageDigest: digest, VMM: types.VMMCloudHypervisor,
		Network: types.NetworkSetup{Backend: types.NetworkBackendCNI, Namespace: base.namespace, Interfaces: []types.NetworkInterface{}},
		State:   types.SandboxStateRunning, Generation: 4, CreatedAt: now, UpdatedAt: now,
	}
	for index := range count {
		added, addErr := provider.Add(t.Context(), fixedID, "bridge", network.AddSpec{Index: index, Queues: 4})
		if addErr != nil {
			t.Fatal(addErr)
		}
		record.Network.Interfaces = append(record.Network.Interfaces, added[0])
		runtime.live[added[0].TAP] = vmm.NetworkDevice{ID: fmt.Sprintf("net-%d", index), TAP: added[0].TAP, MAC: added[0].MAC}
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	service.dependencies.catalog.(*fakeCatalog).record = record
	*steps = nil
	return service, provider, runtime, steps
}

func TestNetResizeGrowsShrinksAndReusesFirstSlot(t *testing.T) {
	service, provider, runtime, steps := newResizeService(t, 1)
	grown, err := service.NetResize(t.Context(), "box", 2)
	if err != nil {
		t.Fatal(err)
	}
	if grown.Config.NICs != 2 || grown.Generation != 4 || len(provider.owned) != 2 || len(runtime.live) != 2 {
		t.Fatalf("grown record = %+v, CNI=%d, VMM=%d", grown, len(provider.owned), len(runtime.live))
	}
	if joined := strings.Join(*steps, ","); !strings.Contains(joined, "network-add,vmm-add,vsock,network-update") {
		t.Fatalf("grow ordering = %v", *steps)
	}
	*steps = nil
	shrunk, err := service.NetResize(t.Context(), "box", 0)
	if err != nil {
		t.Fatal(err)
	}
	if shrunk.Config.NICs != 0 || shrunk.Network.Backend == "" || len(provider.owned) != 0 || len(runtime.live) != 0 {
		t.Fatalf("shrunk record = %+v, CNI=%d, VMM=%d", shrunk, len(provider.owned), len(runtime.live))
	}
	if joined := strings.Join(*steps, ","); !strings.Contains(joined, "vsock,vsock,vmm-remove,network-remove,network-update") {
		t.Fatalf("shrink ordering = %v", *steps)
	}
	if _, err := service.NetResize(t.Context(), "box", 1); err != nil {
		t.Fatalf("readd first NIC: %v", err)
	}
}

func TestCreateZeroNICPreparesHotplugNamespace(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	provider := &resizeNetwork{
		fakeNetwork: &fakeNetwork{steps: steps, namespace: "/var/run/netns/kb-test"},
		owned:       make(map[int]types.NetworkInterface),
	}
	networks, err := network.NewRegistry(provider)
	if err != nil {
		t.Fatal(err)
	}
	service.dependencies.networks = networks
	record, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Network.Backend != types.NetworkBackendCNI || record.Network.Namespace == "" || record.Config.NICs != 0 {
		t.Fatalf("zero-NIC network state = %+v", record)
	}
	if _, err := service.Start(t.Context(), "box"); err != nil {
		t.Fatalf("zero-NIC sandbox cannot start in its reserved namespace: %v", err)
	}
}

func TestNetResizeAddFailureRollsBackCNI(t *testing.T) {
	service, provider, runtime, _ := newResizeService(t, 1)
	runtime.addErr = errors.New("VMM rejected NIC")
	if _, err := service.NetResize(t.Context(), "box", 2); !errors.Is(err, runtime.addErr) {
		t.Fatalf("resize failure = %v", err)
	}
	if len(provider.owned) != 1 || len(runtime.live) != 1 || service.dependencies.catalog.(*fakeCatalog).record.Config.NICs != 1 {
		t.Fatal("failed add changed committed NIC count")
	}
}

func TestNetResizeReportsPartialCommit(t *testing.T) {
	service, provider, runtime, _ := newResizeService(t, 1)
	runtime.addErr = errors.New("third NIC rejected")
	runtime.failAddAt = 2
	record, err := service.NetResize(t.Context(), "box", 3)
	if !errors.Is(err, runtime.addErr) {
		t.Fatalf("resize error = %v", err)
	}
	var classified *errdefs.Error
	if !errors.As(err, &classified) || !classified.Committed {
		t.Fatalf("partial resize not reported as committed: %v", err)
	}
	if record.Config.NICs != 2 || len(provider.owned) != 2 || len(runtime.live) != 2 {
		t.Fatalf("partial resize state = %+v, CNI=%d, VMM=%d", record, len(provider.owned), len(runtime.live))
	}
}

func TestNetResizeReconcilesUncommittedNICBeforeGrowing(t *testing.T) {
	service, provider, runtime, steps := newResizeService(t, 1)
	orphan, err := provider.Add(t.Context(), fixedID, "bridge", network.AddSpec{Index: 1, Queues: 4})
	if err != nil {
		t.Fatal(err)
	}
	runtime.live[orphan[0].TAP] = vmm.NetworkDevice{ID: "uncommitted", TAP: orphan[0].TAP, MAC: orphan[0].MAC}
	*steps = nil
	record, err := service.NetResize(t.Context(), "box", 2)
	if err != nil {
		t.Fatal(err)
	}
	if record.Config.NICs != 2 || len(provider.owned) != 2 || len(runtime.live) != 2 {
		t.Fatalf("reconciled record = %+v, CNI=%d, VMM=%d", record, len(provider.owned), len(runtime.live))
	}
	joined := strings.Join(*steps, ",")
	if !strings.Contains(joined, "vmm-remove,network-remove,network-add,vmm-add") {
		t.Fatalf("orphan reconciliation ordering = %v", *steps)
	}
}

func TestNetResizeRetriesAfterHostRemovalFailure(t *testing.T) {
	service, provider, runtime, _ := newResizeService(t, 1)
	provider.removeErr = errors.New("CNI DEL unavailable")
	if _, err := service.NetResize(t.Context(), "box", 0); !errors.Is(err, provider.removeErr) {
		t.Fatalf("resize failure = %v", err)
	}
	if len(runtime.live) != 0 || len(provider.owned) != 1 || service.dependencies.catalog.(*fakeCatalog).record.Config.NICs != 1 {
		t.Fatal("interrupted remove lost retry state")
	}
	provider.removeErr = nil
	if record, err := service.NetResize(t.Context(), "box", 0); err != nil || record.Config.NICs != 0 {
		t.Fatalf("retry result = %+v, %v", record, err)
	}
}
