package cni

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"

	"github.com/kumabox/kumabox/internal/metadata"
	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/types"
)

// newTestProvider injects host and plugin seams without opening real networks.
func newTestProvider(options Options, store metadata.Store, lists map[string]*libcni.NetworkConfigList, defaultName string, executor pluginRuntime, host platform) *Provider {
	return &Provider{options: options, store: store, lists: lists, defaultName: defaultName, runtime: executor, platform: host}
}

type fakeRuntime struct {
	mu               sync.Mutex
	addError         error
	delError         error
	failInterface    string
	failInterfaceErr error
	adds             []string
	dels             []string
}

func (f *fakeRuntime) AddNetworkList(_ context.Context, _ *libcni.NetworkConfigList, runtime *libcni.RuntimeConf) (cnitypes.Result, error) {
	f.mu.Lock()
	f.adds = append(f.adds, runtime.IfName)
	f.mu.Unlock()
	if f.failInterface != "" && runtime.IfName == f.failInterface {
		return nil, f.failInterfaceErr
	}
	if f.addError != nil {
		return nil, f.addError
	}
	return &current.Result{
		CNIVersion: "1.0.0",
		IPs: []*current.IPConfig{{
			Address: net.IPNet{IP: net.ParseIP("10.42.0.7"), Mask: net.CIDRMask(24, 32)},
			Gateway: net.ParseIP("10.42.0.1"),
		}},
	}, nil
}

func (f *fakeRuntime) DelNetworkList(_ context.Context, _ *libcni.NetworkConfigList, runtime *libcni.RuntimeConf) error {
	f.mu.Lock()
	f.dels = append(f.dels, runtime.IfName)
	f.mu.Unlock()
	return f.delError
}

type fakePlatform struct {
	mu              sync.Mutex
	namespace       bool
	removeError     error
	linksUp         []bool
	deletedTAPs     []string
	verifiedTAPs    []string
	ensuredNames    []string
	removedNames    []string
	redirectedNames []string
}

func (f *fakePlatform) EnsureNamespace(name, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	created := !f.namespace
	f.namespace = true
	f.ensuredNames = append(f.ensuredNames, name)
	return created, nil
}

func (f *fakePlatform) RemoveNamespace(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedNames = append(f.removedNames, name)
	if f.removeError != nil {
		return f.removeError
	}
	f.namespace = false
	return nil
}

func (f *fakePlatform) NamespaceExists(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.namespace {
		return os.ErrNotExist
	}
	return nil
}

func (f *fakePlatform) SetupRedirect(_, interfaceName, _ string, _ int, overrideMAC string) (string, error) {
	f.mu.Lock()
	f.redirectedNames = append(f.redirectedNames, interfaceName)
	f.mu.Unlock()
	if overrideMAC != "" {
		return overrideMAC, nil
	}
	return "02:00:00:00:00:07", nil
}

func (f *fakePlatform) DeleteTAP(_, tap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedTAPs = append(f.deletedTAPs, tap)
	return nil
}

func (f *fakePlatform) SetLinkState(_ string, _ []string, up bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linksUp = append(f.linksUp, up)
	return nil
}

func (f *fakePlatform) VerifyTAP(_, tap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifiedTAPs = append(f.verifiedTAPs, tap)
	return nil
}

func TestProviderLifecyclePersistsCleanupIntent(t *testing.T) {
	provider, executor, host, id := testProvider(t)
	namespace, err := provider.Prepare(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if namespace != filepath.Join(namedNamespaceDir, "kb-"+id.String()) {
		t.Fatalf("namespace = %q", namespace)
	}
	interfaces, err := provider.Add(t.Context(), id, "bridge", network.AddSpec{Index: 0, Queues: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(interfaces) != 1 || interfaces[0].MAC != "02:00:00:00:00:07" || interfaces[0].IPv4.Address != "10.42.0.7" {
		t.Fatalf("interfaces = %+v", interfaces)
	}
	record, err := provider.view(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if record == nil || record.Phase != phaseReady || record.Interfaces[0].Phase != interfaceReady {
		t.Fatalf("record = %+v", record)
	}
	if err := provider.Quiesce(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := provider.Unquiesce(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := provider.Verify(t.Context(), id, interfaces); err != nil {
		t.Fatal(err)
	}
	if err := provider.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	record, err = provider.view(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if record != nil || host.namespace {
		t.Fatalf("delete retained record=%+v namespace=%t", record, host.namespace)
	}
	if !slices.Equal(executor.adds, []string{"eth0"}) || !slices.Equal(executor.dels, []string{"eth0"}) {
		t.Fatalf("CNI calls add=%v del=%v", executor.adds, executor.dels)
	}
	if !slices.Equal(host.linksUp, []bool{false, true}) {
		t.Fatalf("link states = %v", host.linksUp)
	}
}

func TestAddMultipleInterfaces(t *testing.T) {
	provider, executor, host, id := testProvider(t)
	if _, err := provider.Prepare(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	interfaces, err := provider.Add(t.Context(), id, "bridge",
		network.AddSpec{Index: 0, Queues: 4},
		network.AddSpec{Index: 1, Queues: 4},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(interfaces) != 2 || interfaces[0].Index != 0 || interfaces[1].Index != 1 {
		t.Fatalf("interfaces = %+v", interfaces)
	}
	adds := append([]string(nil), executor.adds...)
	slices.Sort(adds)
	if !slices.Equal(adds, []string{"eth0", "eth1"}) {
		t.Fatalf("CNI adds = %v", adds)
	}
	redirects := append([]string(nil), host.redirectedNames...)
	slices.Sort(redirects)
	if !slices.Equal(redirects, []string{"eth0", "eth1"}) {
		t.Fatalf("redirects = %v", redirects)
	}
	record, err := provider.view(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if record == nil || record.Phase != phaseReady || len(record.Interfaces) != 2 {
		t.Fatalf("record = %+v", record)
	}
	for _, item := range record.Interfaces {
		if item.Phase != interfaceReady {
			t.Fatalf("interface %d phase = %s", item.Index, item.Phase)
		}
	}
}

func TestAddFailureRollsBackEveryNIC(t *testing.T) {
	provider, executor, _, id := testProvider(t)
	if _, err := provider.Prepare(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("second NIC failed")
	executor.failInterface = "eth1"
	executor.failInterfaceErr = failure
	if _, err := provider.Add(t.Context(), id, "bridge",
		network.AddSpec{Index: 0, Queues: 4},
		network.AddSpec{Index: 1, Queues: 4},
	); !errors.Is(err, failure) {
		t.Fatalf("Add error = %v, want %v", err, failure)
	}
	dels := append([]string(nil), executor.dels...)
	slices.Sort(dels)
	if !slices.Equal(dels, []string{"eth0", "eth1"}) {
		t.Fatalf("rollback CNI dels = %v", dels)
	}
}

func TestRemoveNICRetainsNamespaceAndCanReadd(t *testing.T) {
	provider, executor, host, id := testProvider(t)
	if _, err := provider.Prepare(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	first, err := provider.Add(t.Context(), id, "bridge", network.AddSpec{Index: 0, Queues: 4})
	if err != nil {
		t.Fatal(err)
	}
	if index, ok := provider.IndexForTAP(id, first[0].TAP); !ok || index != 0 {
		t.Fatalf("TAP ownership = %d, %t", index, ok)
	}
	if _, ok := provider.IndexForTAP(types.SandboxID("223e4567-e89b-42d3-a456-426614174000"), first[0].TAP); ok {
		t.Fatal("accepted another sandbox's TAP")
	}
	if err := provider.Remove(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	if err := provider.Remove(t.Context(), id, 0); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
	indices, err := provider.Allocated(t.Context(), id)
	if err != nil || len(indices) != 0 || !host.namespace {
		t.Fatalf("remaining indices = %v, namespace = %t, err = %v", indices, host.namespace, err)
	}
	if !slices.Equal(executor.dels, []string{"eth0"}) || !slices.Equal(host.deletedTAPs, []string{first[0].TAP}) {
		t.Fatalf("released CNI=%v TAP=%v", executor.dels, host.deletedTAPs)
	}
	if _, err := provider.Add(t.Context(), id, "bridge", network.AddSpec{Index: 0, Queues: 4}); err != nil {
		t.Fatalf("readd after removal: %v", err)
	}
}

func TestRemoveNICRetainsIntentAfterCNIError(t *testing.T) {
	provider, executor, _, id := testProvider(t)
	if _, err := provider.Add(t.Context(), id, "bridge", network.AddSpec{Index: 0, Queues: 4}); err != nil {
		t.Fatal(err)
	}
	executor.delError = errors.New("plugin unavailable")
	if err := provider.Remove(t.Context(), id, 0); err == nil {
		t.Fatal("remove succeeded despite CNI DEL failure")
	}
	record, err := provider.view(t.Context(), id)
	if err != nil || record.Interfaces[0].Phase != interfaceDeleting {
		t.Fatalf("retained cleanup intent = %+v, %v", record, err)
	}
	executor.delError = nil
	if err := provider.Remove(t.Context(), id, 0); err != nil {
		t.Fatalf("retry removal: %v", err)
	}
}

func TestAddFailureCompensatesWithoutLosingNamespaceOwnership(t *testing.T) {
	provider, executor, _, id := testProvider(t)
	executor.addError = errors.New("injected ADD failure")
	if _, err := provider.Prepare(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Add(t.Context(), id, "bridge", network.AddSpec{Index: 0, Queues: 2}); err == nil {
		t.Fatal("Add unexpectedly succeeded")
	}
	record, err := provider.view(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if record == nil || record.Phase != phasePreparing || len(record.Interfaces) != 0 {
		t.Fatalf("rollback record = %+v", record)
	}
	if !slices.Equal(executor.dels, []string{"eth0"}) {
		t.Fatalf("rollback DEL calls = %v", executor.dels)
	}
}

func TestDeleteFailureRetainsOnlyRetryableCleanupState(t *testing.T) {
	provider, executor, _, id := testProvider(t)
	if _, err := provider.Prepare(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	interfaces, err := provider.Add(t.Context(), id, "bridge", network.AddSpec{Index: 0, Queues: 2})
	if err != nil || len(interfaces) != 1 {
		t.Fatalf("Add = %+v, %v", interfaces, err)
	}
	executor.delError = errors.New("injected DEL failure")
	if err := provider.Delete(t.Context(), id); err == nil {
		t.Fatal("Delete unexpectedly succeeded")
	}
	record, err := provider.view(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if record == nil || record.Phase != phaseDeleting || len(record.Interfaces) != 1 {
		t.Fatalf("failed delete record = %+v", record)
	}
	executor.delError = nil
	if err := provider.Delete(t.Context(), id); err != nil {
		t.Fatalf("Delete retry: %v", err)
	}
	if record, err := provider.view(t.Context(), id); err != nil || record != nil {
		t.Fatalf("retry retained record=%+v error=%v", record, err)
	}
}

func TestLoadConfListsUsesFirstFilenameAndRejectsDuplicateNames(t *testing.T) {
	directory := t.TempDir()
	writeConflist(t, directory, "20-second.conflist", "second")
	writeConflist(t, directory, "10-first.conflist", "first")
	lists, defaultName, err := loadConfLists(directory)
	if err != nil {
		t.Fatal(err)
	}
	if defaultName != "first" || len(lists) != 2 {
		t.Fatalf("default=%q lists=%v", defaultName, lists)
	}
	writeConflist(t, directory, "30-duplicate.conflist", "first")
	if _, _, err := loadConfLists(directory); err == nil {
		t.Fatal("duplicate CNI network name was accepted")
	}
}

func TestNewWithoutConflistAllowsInspectionButRejectsAdd(t *testing.T) {
	store, err := metadata.NewMemory(Collections())
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		ConfDir: filepath.Join(t.TempDir(), "missing"), BinDir: "/opt/cni/bin",
		CacheDir: filepath.Join(t.TempDir(), "cache"), NamespacePrefix: "kb-", CleanupTimeout: time.Second,
	}
	provider, err := New(options, store)
	if err != nil {
		t.Fatal(err)
	}
	id := mustID(t)
	if namespace, err := provider.Prepare(t.Context(), id); err != nil || namespace != "" {
		t.Fatalf("Prepare = %q, %v", namespace, err)
	}
	if _, err := provider.Add(t.Context(), id, "", network.AddSpec{Index: 0, Queues: 2}); !errors.Is(err, network.ErrNotConfigured) {
		t.Fatalf("Add error = %v", err)
	}
}

func testProvider(t *testing.T) (*Provider, *fakeRuntime, *fakePlatform, types.SandboxID) {
	t.Helper()
	store, err := metadata.NewMemory(Collections())
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeRuntime{}
	host := &fakePlatform{}
	list := &libcni.NetworkConfigList{Name: "bridge", CNIVersion: "1.0.0"}
	options := Options{
		ConfDir: "/etc/cni/net.d", BinDir: "/opt/cni/bin", CacheDir: filepath.Join(t.TempDir(), "cache"),
		NamespacePrefix: "kb-", CleanupTimeout: time.Second,
	}
	return newTestProvider(options, store, map[string]*libcni.NetworkConfigList{"bridge": list}, "bridge", executor, host), executor, host, mustID(t)
}

func mustID(t *testing.T) types.SandboxID {
	t.Helper()
	id, err := types.ParseSandboxID("123e4567-e89b-42d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func writeConflist(t *testing.T, directory, name, networkName string) {
	t.Helper()
	contents := []byte(`{"cniVersion":"1.0.0","name":"` + networkName + `","plugins":[{"type":"bridge"}]}`)
	if err := os.WriteFile(filepath.Join(directory, name), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}
