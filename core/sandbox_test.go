package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/config"
	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
	"github.com/kumabox/kumabox/network"
	"github.com/kumabox/kumabox/sandbox"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
	"github.com/kumabox/kumabox/vmm"
)

var fixedID = types.SandboxID("123e4567-e89b-42d3-a456-426614174000")

type fakeGuard struct {
	image    types.Image
	steps    *[]string
	afterUse error
}

func (f fakeGuard) WithAvailable(ctx context.Context, _ string, use func(types.Image) error) (types.Image, error) {
	*f.steps = append(*f.steps, "verify")
	if err := use(f.image); err != nil {
		return types.Image{}, err
	}
	return f.image, f.afterUse
}

type fakeCatalog struct {
	steps   *[]string
	record  types.Sandbox
	deleted bool
}

func (f *fakeCatalog) Reserve(_ context.Context, _ string, _ types.Digest, record types.Sandbox) error {
	*f.steps = append(*f.steps, "reserve")
	f.record = record
	return nil
}

func (f *fakeCatalog) MarkCreated(_ context.Context, _ types.SandboxID, expected uint64, setup types.NetworkSetup, updated time.Time) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "created")
	if expected != f.record.Generation {
		return types.Sandbox{}, errors.New("wrong generation")
	}
	f.record.Network = setup
	if len(setup.Interfaces) > 0 {
		f.record.Config.NetworkName = setup.Interfaces[0].Network
	}
	f.record.State, f.record.Generation, f.record.UpdatedAt = types.SandboxStateCreated, expected+1, updated
	return f.record, nil
}

func (f *fakeCatalog) MarkError(_ context.Context, _ types.SandboxID, _ uint64, failure types.SandboxFailure, updated time.Time) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "error")
	f.record.State, f.record.Failure, f.record.UpdatedAt = types.SandboxStateError, &failure, updated
	f.record.Generation++
	return f.record, nil
}

func (f *fakeCatalog) BeginStart(_ context.Context, _ types.SandboxID, expected uint64, updated time.Time) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "starting")
	if f.record.Generation != expected {
		return types.Sandbox{}, errors.New("wrong generation")
	}
	if f.record.State == types.SandboxStateStarting {
		return f.record, nil
	}
	f.record.State, f.record.Generation, f.record.Failure = types.SandboxStateStarting, expected+1, nil
	f.record.UpdatedAt = updated
	return f.record, nil
}

func (f *fakeCatalog) MarkRunning(_ context.Context, _ types.SandboxID, expected uint64, updated time.Time) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "running")
	if f.record.State != types.SandboxStateStarting || f.record.Generation != expected {
		return types.Sandbox{}, errors.New("wrong starting generation")
	}
	f.record.State, f.record.Generation, f.record.UpdatedAt = types.SandboxStateRunning, expected+1, updated
	return f.record, nil
}

func (f *fakeCatalog) MarkStartError(_ context.Context, _ types.SandboxID, expected uint64, failure types.SandboxFailure, updated time.Time) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "start-error")
	if f.record.State != types.SandboxStateStarting || f.record.Generation != expected {
		return types.Sandbox{}, errors.New("wrong starting generation")
	}
	f.record.State, f.record.Generation, f.record.Failure = types.SandboxStateError, expected+1, &failure
	f.record.UpdatedAt = updated
	return f.record, nil
}

func (f *fakeCatalog) BeginStop(_ context.Context, _ types.SandboxID, expected uint64, updated time.Time) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "stopping")
	if f.record.Generation != expected {
		return types.Sandbox{}, errors.New("wrong generation")
	}
	if f.record.State == types.SandboxStateStopping {
		return f.record, nil
	}
	if f.record.State != types.SandboxStateRunning {
		return types.Sandbox{}, errors.New("wrong running state")
	}
	f.record.State, f.record.Generation, f.record.UpdatedAt = types.SandboxStateStopping, expected+1, updated
	return f.record, nil
}

func (f *fakeCatalog) MarkStopped(_ context.Context, _ types.SandboxID, expected uint64, from types.SandboxState, updated time.Time) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "stopped")
	if f.record.State != from || f.record.Generation != expected {
		return types.Sandbox{}, errors.New("wrong stoppable generation")
	}
	f.record.State, f.record.Generation, f.record.UpdatedAt = types.SandboxStateStopped, expected+1, updated
	return f.record, nil
}

func (f *fakeCatalog) Forget(context.Context, types.SandboxID, uint64) error {
	*f.steps = append(*f.steps, "forget")
	return nil
}

func (f *fakeCatalog) Resolve(_ context.Context, _ string) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "resolve")
	if f.deleted {
		return types.Sandbox{}, errors.New("not found")
	}
	return f.record, nil
}

func (f *fakeCatalog) List(context.Context) ([]types.Sandbox, error) {
	*f.steps = append(*f.steps, "list")
	if f.deleted || f.record.ID == "" {
		return []types.Sandbox{}, nil
	}
	return []types.Sandbox{f.record}, nil
}

func (f *fakeCatalog) BeginDelete(_ context.Context, _ types.SandboxID, expected uint64, updated time.Time) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "deleting")
	if f.record.Generation != expected {
		return types.Sandbox{}, errors.New("wrong generation")
	}
	if f.record.State == types.SandboxStateDeleting {
		return f.record, nil
	}
	switch f.record.State {
	case types.SandboxStateCreating, types.SandboxStateCreated, types.SandboxStateStopped, types.SandboxStateError:
	default:
		return types.Sandbox{}, errdefs.New(errdefs.ClassConflict, errdefs.CodeStateConflict, errors.New("sandbox must be stopped before removal"))
	}
	f.record.State, f.record.Generation = types.SandboxStateDeleting, f.record.Generation+1
	f.record.Failure = nil
	f.record.UpdatedAt = updated
	return f.record, nil
}

func (f *fakeCatalog) FinalizeDelete(_ context.Context, _ types.SandboxID, expected uint64) error {
	*f.steps = append(*f.steps, "finalize")
	if f.record.State != types.SandboxStateDeleting || f.record.Generation != expected {
		return errors.New("wrong delete generation")
	}
	f.deleted = true
	return nil
}

type fakeDisk struct {
	steps   *[]string
	prepare error
	remove  error
}

type fakeNetwork struct {
	steps      *[]string
	prepareErr error
	addErr     error
	recoverErr error
	quiesceErr error
	deleteErr  error
	namespace  string
	interfaces []types.NetworkInterface
	recovered  []types.NetworkInterface
	specs      []network.AddSpec
}

func (*fakeNetwork) Type() types.NetworkBackend { return types.NetworkBackendCNI }

func (f *fakeNetwork) Prepare(context.Context, types.SandboxID) (string, error) {
	*f.steps = append(*f.steps, "network-prepare")
	return f.namespace, f.prepareErr
}

func (f *fakeNetwork) Add(_ context.Context, _ types.SandboxID, networkName string, specs ...network.AddSpec) ([]types.NetworkInterface, error) {
	*f.steps = append(*f.steps, "network-add")
	f.specs = append([]network.AddSpec(nil), specs...)
	if f.addErr != nil {
		return nil, f.addErr
	}
	if len(f.interfaces) > 0 {
		return append([]types.NetworkInterface(nil), f.interfaces...), nil
	}
	if networkName == "" {
		networkName = "default"
	}
	result := make([]types.NetworkInterface, 0, len(specs))
	for _, spec := range specs {
		result = append(result, types.NetworkInterface{
			Index: spec.Index, Name: fmt.Sprintf("eth%d", spec.Index), TAP: fmt.Sprintf("tap%d", spec.Index),
			MAC: fmt.Sprintf("02:00:00:00:00:%02x", spec.Index+1), Queues: spec.Queues,
			QueueSize: network.DefaultQueueSize, Network: networkName,
		})
	}
	return result, nil
}

func (*fakeNetwork) Verify(context.Context, types.SandboxID, []types.NetworkInterface) error {
	return nil
}

func (f *fakeNetwork) Recover(_ context.Context, _ types.SandboxID, _ string, expected []types.NetworkInterface) ([]types.NetworkInterface, error) {
	*f.steps = append(*f.steps, "network-recover")
	if f.recovered != nil {
		return append([]types.NetworkInterface(nil), f.recovered...), f.recoverErr
	}
	return append([]types.NetworkInterface(nil), expected...), f.recoverErr
}

func (f *fakeNetwork) Quiesce(context.Context, types.SandboxID) error {
	*f.steps = append(*f.steps, "network-quiesce")
	return f.quiesceErr
}

func (f *fakeNetwork) Unquiesce(context.Context, types.SandboxID) error {
	*f.steps = append(*f.steps, "network-unquiesce")
	return nil
}

func (f *fakeNetwork) Delete(context.Context, types.SandboxID) error {
	*f.steps = append(*f.steps, "network-delete")
	return f.deleteErr
}

func (f fakeDisk) Prepare(context.Context, types.SandboxID, int64) error {
	*f.steps = append(*f.steps, "disk")
	return f.prepare
}

func (f fakeDisk) Check(context.Context, types.SandboxID, int64) error {
	*f.steps = append(*f.steps, "check")
	return nil
}

func (f fakeDisk) Remove(context.Context, types.SandboxID) error {
	*f.steps = append(*f.steps, "remove")
	return f.remove
}

type fakeReporter struct{ steps *[]string }

func (f fakeReporter) Status(status string) error {
	*f.steps = append(*f.steps, "status:"+status)
	return nil
}

func (f fakeReporter) Committed(types.Sandbox) error {
	*f.steps = append(*f.steps, "report")
	return nil
}

type fakeRuntime struct {
	typ           types.VMMType
	steps         *[]string
	observation   vmm.Observation
	preflightErr  error
	launchErr     error
	stopErr       error
	plan          vmm.LaunchPlan
	console       io.ReadWriteCloser
	vsock         io.ReadWriteCloser
	logs          string
	logsErr       error
	removeLogsErr error
	logOptions    vmm.LogOptions
	snapshotPlan  vmm.SnapshotPlan
	snapshotErr   error
	restorePlan   vmm.RestorePlan
	restoreErr    error
}

func (f *fakeRuntime) Snapshot(_ context.Context, plan vmm.SnapshotPlan) error {
	*f.steps = append(*f.steps, "snapshot")
	f.snapshotPlan = plan
	if f.snapshotErr != nil {
		return f.snapshotErr
	}
	if err := os.WriteFile(filepath.Join(plan.Destination, "config.json"), []byte("{}"), 0o600); err != nil {
		return err
	}
	for _, file := range plan.WritableFiles {
		if err := os.WriteFile(file.Destination, []byte("cow"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeRuntime) Restore(_ context.Context, plan vmm.RestorePlan) (vmm.Process, error) {
	*f.steps = append(*f.steps, "restore")
	f.restorePlan = plan
	process := vmm.Process{
		PID: 43, StartTicks: 11, BootID: "boot", SandboxID: plan.SandboxID,
		Generation: plan.Generation, Binary: "cloud-hypervisor", APISocket: "/run/kumabox/restore.sock",
	}
	return process, f.restoreErr
}

func (f *fakeRuntime) Type() types.VMMType {
	if f.typ == "" {
		return types.VMMCloudHypervisor
	}
	return f.typ
}

func (f *fakeRuntime) Preflight() error {
	*f.steps = append(*f.steps, "preflight")
	return f.preflightErr
}

func (f *fakeRuntime) Locate(context.Context, types.SandboxID, uint64) (vmm.Process, bool, error) {
	*f.steps = append(*f.steps, "locate")
	return f.observation.Process, f.observation.State != vmm.ProcessAbsent, nil
}

func (f *fakeRuntime) Observe(context.Context, types.SandboxID, uint64) (vmm.Observation, error) {
	*f.steps = append(*f.steps, "observe")
	return f.observation, nil
}

func (f *fakeRuntime) WaitReady(context.Context, vmm.Process) error {
	*f.steps = append(*f.steps, "ready")
	return nil
}

func (f *fakeRuntime) Launch(_ context.Context, plan vmm.LaunchPlan) (vmm.Process, error) {
	*f.steps = append(*f.steps, "launch")
	f.plan = plan
	process := vmm.Process{
		PID: 42, StartTicks: 10, BootID: "boot", SandboxID: plan.SandboxID,
		Generation: plan.Generation, Binary: "cloud-hypervisor", APISocket: "/run/kumabox/api.sock",
	}
	return process, f.launchErr
}

func (f *fakeRuntime) Abort(context.Context, vmm.Process) error {
	*f.steps = append(*f.steps, "abort")
	return nil
}

func (f *fakeRuntime) Stop(context.Context, vmm.Process) error {
	*f.steps = append(*f.steps, "stop")
	return f.stopErr
}

func (f *fakeRuntime) Console(context.Context, vmm.Process) (io.ReadWriteCloser, error) {
	*f.steps = append(*f.steps, "console")
	if f.console == nil {
		f.console = &fakeConsole{}
	}
	return f.console, nil
}

func (f *fakeRuntime) DialVsock(context.Context, vmm.Process, uint32) (io.ReadWriteCloser, error) {
	*f.steps = append(*f.steps, "vsock")
	if f.vsock == nil {
		return nil, errors.New("fake vsock is not configured")
	}
	return f.vsock, nil
}

func (f *fakeRuntime) Logs(_ context.Context, _ types.SandboxID, options vmm.LogOptions, output io.Writer) error {
	*f.steps = append(*f.steps, "logs")
	f.logOptions = options
	if f.logsErr != nil {
		return f.logsErr
	}
	_, err := io.WriteString(output, f.logs)
	return err
}

func (f *fakeRuntime) Cleanup(context.Context, types.SandboxID) error {
	*f.steps = append(*f.steps, "cleanup")
	return nil
}

func (f *fakeRuntime) RemoveLogs(context.Context, types.SandboxID) error {
	*f.steps = append(*f.steps, "remove-logs")
	return f.removeLogsErr
}

type fakeConsole struct{ closed bool }

func (*fakeConsole) Read([]byte) (int, error)       { return 0, io.EOF }
func (*fakeConsole) Write(data []byte) (int, error) { return len(data), nil }
func (f *fakeConsole) Close() error {
	f.closed = true
	return nil
}

func newTestSandboxService(t *testing.T, diskError error) (*SandboxService, *[]string) {
	t.Helper()
	digest, err := types.ParseDigest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	roots := storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	paths, err := sandbox.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	steps := []string{}
	catalog := &fakeCatalog{steps: &steps}
	imagePaths, err := images.NewPaths(roots)
	if err != nil {
		t.Fatal(err)
	}
	image := types.Image{
		ManifestDigest: digest, Platform: types.Platform{OS: "linux", Architecture: runtime.GOARCH},
		Layers: []types.Layer{{SourceDigest: digest}},
		Boot:   types.Boot{Profile: types.BootProfileOverlayV1, KernelLayer: digest, InitrdLayer: digest, KernelFile: "vmlinuz", InitrdFile: "initrd.img"},
	}
	runtimeAdapter := &fakeRuntime{steps: &steps, observation: vmm.Observation{State: vmm.ProcessAbsent}}
	runtimes, err := vmm.NewRegistry(runtimeAdapter)
	if err != nil {
		t.Fatal(err)
	}
	networkAdapter := &fakeNetwork{steps: &steps, namespace: "/var/run/netns/kumabox-test"}
	networks, err := network.NewRegistry(networkAdapter)
	if err != nil {
		t.Fatal(err)
	}
	service, err := newSandboxService(sandboxDependencies{
		paths: paths, imagePaths: imagePaths, images: fakeGuard{image: image, steps: &steps},
		catalog: catalog, disks: fakeDisk{steps: &steps, prepare: diskError},
		networks: networks, runtimes: runtimes,
		defaultVMM: types.VMMCloudHypervisor, cleanupTimeout: 10 * time.Second,
		defaultNetwork: types.NetworkBackendCNI,
		reporter:       fakeReporter{steps: &steps},
		newID:          func() (types.SandboxID, error) { return fixedID, nil },
		now:            func() time.Time { return time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, &steps
}

func testNetwork(t *testing.T, service *SandboxService) *fakeNetwork {
	t.Helper()
	provider, err := service.dependencies.networks.Provider(types.NetworkBackendCNI)
	if err != nil {
		t.Fatal(err)
	}
	networkAdapter, ok := provider.(*fakeNetwork)
	if !ok {
		t.Fatalf("network provider = %T, want *fakeNetwork", provider)
	}
	return networkAdapter
}

func testRuntime(t *testing.T, service *SandboxService) *fakeRuntime {
	t.Helper()
	backend, err := service.dependencies.runtimes.Backend(types.VMMCloudHypervisor)
	if err != nil {
		t.Fatal(err)
	}
	runtimeAdapter, ok := backend.(*fakeRuntime)
	if !ok {
		t.Fatalf("runtime backend = %T, want *fakeRuntime", backend)
	}
	return runtimeAdapter
}

func TestOpenVMMRegistryUsesConfiguredCgroupParent(t *testing.T) {
	configuration := config.Default()
	base := t.TempDir()
	configuration.Paths = storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	}
	configuration.VMM.CgroupParent = filepath.Join(base, "outside-cgroup")
	if _, err := openVMMRegistry(configuration); err == nil {
		t.Fatal("openVMMRegistry() ignored the configured cgroup parent")
	}
}

func TestNewSandboxServiceValidatesNamedDependencies(t *testing.T) {
	service, _ := newTestSandboxService(t, nil)
	valid := service.dependencies
	for _, test := range []struct {
		name   string
		mutate func(*sandboxDependencies)
	}{
		{name: "image guard", mutate: func(dependencies *sandboxDependencies) { dependencies.images = nil }},
		{name: "catalog", mutate: func(dependencies *sandboxDependencies) { dependencies.catalog = nil }},
		{name: "disk backend", mutate: func(dependencies *sandboxDependencies) { dependencies.disks = nil }},
		{name: "network provider", mutate: func(dependencies *sandboxDependencies) { dependencies.networks = nil }},
		{name: "VMM registry", mutate: func(dependencies *sandboxDependencies) { dependencies.runtimes = nil }},
		{name: "cleanup timeout", mutate: func(dependencies *sandboxDependencies) { dependencies.cleanupTimeout = 0 }},
		{name: "default VMM", mutate: func(dependencies *sandboxDependencies) { dependencies.defaultVMM = types.VMMFirecracker }},
		{name: "default network", mutate: func(dependencies *sandboxDependencies) { dependencies.defaultNetwork = "missing" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dependencies := valid
			test.mutate(&dependencies)
			if _, err := newSandboxService(dependencies); err == nil {
				t.Fatal("newSandboxService() accepted incomplete dependencies")
			}
		})
	}
}

func TestNewSandboxServiceSuppliesProcessLocalDefaults(t *testing.T) {
	service, _ := newTestSandboxService(t, nil)
	dependencies := service.dependencies
	dependencies.reporter = nil
	dependencies.newID = nil
	dependencies.now = nil
	configured, err := newSandboxService(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if configured.dependencies.reporter == nil || configured.dependencies.newID == nil || configured.dependencies.now == nil {
		t.Fatal("newSandboxService() left process-local defaults unconfigured")
	}
}

func TestListFiltersInactiveSandboxesUnlessAllRequested(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	*steps = nil
	if records, err := service.List(t.Context(), false); err != nil {
		t.Fatal(err)
	} else if len(records) != 0 {
		t.Fatalf("active records = %+v, want none", records)
	}
	if records, err := service.List(t.Context(), true); err != nil {
		t.Fatal(err)
	} else if len(records) != 1 || records[0].ID != fixedID {
		t.Fatalf("all records = %+v", records)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record.State = types.SandboxStateRunning
	if records, err := service.List(t.Context(), false); err != nil {
		t.Fatal(err)
	} else if len(records) != 1 || records[0].State != types.SandboxStateRunning {
		t.Fatalf("running records = %+v", records)
	}
}

func TestInspectReturnsResolvedPersistentRecord(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	*steps = nil
	record, err := service.Inspect(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != fixedID || record.Config.Name != "box" || record.State != types.SandboxStateCreated {
		t.Fatalf("Inspect = %+v", record)
	}
	if diff := strings.Join(*steps, ","); diff != "resolve" {
		t.Fatalf("steps = %q, want resolve", diff)
	}
}
