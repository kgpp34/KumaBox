package core

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/images"
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

func (f *fakeCatalog) MarkCreated(_ context.Context, _ types.SandboxID, expected uint64, updated time.Time) (types.Sandbox, error) {
	*f.steps = append(*f.steps, "created")
	if expected != f.record.Generation {
		return types.Sandbox{}, errors.New("wrong generation")
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
	steps        *[]string
	observation  vmm.Observation
	preflightErr error
	launchErr    error
	stopErr      error
	plan         vmm.LaunchPlan
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

func (f *fakeRuntime) Cleanup(context.Context, types.SandboxID) error {
	*f.steps = append(*f.steps, "cleanup")
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
	service := newSandboxService(paths, imagePaths, fakeGuard{image: image, steps: &steps}, catalog, catalog, catalog, catalog, fakeDisk{steps: &steps, prepare: diskError}, runtimeAdapter, fakeReporter{steps: &steps})
	service.newID = func() (types.SandboxID, error) { return fixedID, nil }
	service.now = func() time.Time { return time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC) }
	return service, &steps
}

func TestCreateCommitsCreatedAfterDiskPreparation(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	record, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != fixedID || record.State != types.SandboxStateCreated || record.Generation != 2 {
		t.Fatalf("created record = %+v", record)
	}
	want := []string{"status:resolving and checking image", "verify", "reserve", "status:creating sparse ext4 disk", "disk", "status:committing created state", "created", "report"}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestCreateDiskFailureRemovesDiskBeforeForgettingReservation(t *testing.T) {
	failure := errors.New("mkfs failed")
	service, steps := newTestSandboxService(t, failure)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); !errors.Is(err, failure) {
		t.Fatalf("Create error = %v", err)
	}
	wantTail := []string{"disk", "remove", "forget"}
	if got := (*steps)[len(*steps)-len(wantTail):]; !reflect.DeepEqual(got, wantTail) {
		t.Fatalf("cleanup steps = %v, want %v", got, wantTail)
	}
}

func TestCreateImageUnlockFailureCompensatesCommittedReservation(t *testing.T) {
	failure := errors.New("image lock close failed")
	service, steps := newTestSandboxService(t, nil)
	guard := service.images.(fakeGuard)
	guard.afterUse = failure
	service.images = guard
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); !errors.Is(err, failure) {
		t.Fatalf("Create error = %v", err)
	}
	want := []string{"status:resolving and checking image", "verify", "reserve", "remove", "forget"}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestCreateRetainsErrorOwnerWhenDiskCleanupFails(t *testing.T) {
	prepareFailure := errors.New("mkfs failed")
	removeFailure := errors.New("disk cleanup failed")
	service, steps := newTestSandboxService(t, prepareFailure)
	disks := service.cows.(fakeDisk)
	disks.remove = removeFailure
	service.cows = disks
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); !errors.Is(err, prepareFailure) || !errors.Is(err, removeFailure) {
		t.Fatalf("Create error = %v", err)
	}
	catalog := service.creator.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateError || catalog.record.Failure == nil || catalog.record.Failure.Phase != "disk" {
		t.Fatalf("retained record = %+v", catalog.record)
	}
	wantTail := []string{"disk", "remove", "error"}
	if got := (*steps)[len(*steps)-len(wantTail):]; !reflect.DeepEqual(got, wantTail) {
		t.Fatalf("cleanup steps = %v, want %v", got, wantTail)
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
	catalog := service.reader.(*fakeCatalog)
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

func TestStartCommitsRunningOnlyAfterLaunchReadiness(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	*steps = nil
	record, err := service.Start(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateRunning || record.Generation != 4 {
		t.Fatalf("running record = %+v", record)
	}
	runtimeAdapter := service.runtime.(*fakeRuntime)
	if runtimeAdapter.plan.Generation != 3 || len(runtimeAdapter.plan.Disks) != 2 || runtimeAdapter.plan.Disks[0].Serial != "kumabox-layer0" || runtimeAdapter.plan.Disks[1].Serial != vmm.COWSerial {
		t.Fatalf("launch plan = %+v", runtimeAdapter.plan)
	}
	want := []string{
		"status:resolving sandbox", "resolve", "status:waiting for sandbox operation lock", "resolve",
		"status:checking existing runtime", "observe", "cleanup", "status:checking host runtime", "preflight",
		"status:verifying image and sandbox disk", "verify", "check", "status:committing starting state", "starting",
		"status:launching Cloud Hypervisor", "launch", "status:committing running state", "running", "report",
	}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestStartRecoversRunningProcessFromStartingState(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.lifecycle.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateStarting, 3
	runtimeAdapter := service.runtime.(*fakeRuntime)
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42}}
	*steps = nil
	record, err := service.Start(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateRunning || record.Generation != 4 {
		t.Fatalf("recovered record = %+v", record)
	}
	if strings.Contains(strings.Join(*steps, ","), "launch") || strings.Contains(strings.Join(*steps, ","), "preflight") {
		t.Fatalf("recovery relaunched VMM: %v", *steps)
	}
}

func TestStartFailureAbortsProcessAndRetainsError(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("VMM exited")
	service.runtime.(*fakeRuntime).launchErr = failure
	*steps = nil
	if _, err := service.Start(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Start error = %v", err)
	} else {
		var classified *errdefs.Error
		if !errors.As(err, &classified) || !classified.Committed {
			t.Fatalf("Start did not report retained state: %v", err)
		}
	}
	catalog := service.lifecycle.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateError || catalog.record.Failure == nil || catalog.record.Failure.Phase != "launch VMM" {
		t.Fatalf("failed start record = %+v", catalog.record)
	}
	joined := strings.Join(*steps, ",")
	if !strings.Contains(joined, "launch,abort,start-error") {
		t.Fatalf("process was not aborted before Error commit: %v", *steps)
	}
}

func TestStartRetryDoesNotLeaveStartingAfterPreflightFailure(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.lifecycle.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateStarting, 3
	failure := errors.New("KVM unavailable")
	service.runtime.(*fakeRuntime).preflightErr = failure
	*steps = nil
	if _, err := service.Start(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Start error = %v", err)
	}
	if catalog.record.State != types.SandboxStateError || catalog.record.Failure == nil || catalog.record.Failure.Phase != "host preflight" {
		t.Fatalf("failed recovery record = %+v", catalog.record)
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got, "observe,cleanup,status:checking host runtime,preflight,cleanup,start-error") {
		t.Fatalf("recovery steps = %v", *steps)
	}
}

func TestStopRecordsIntentBeforeTerminatingRunningVMM(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.lifecycle.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	service.runtime.(*fakeRuntime).observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42}}
	*steps = nil
	record, err := service.Stop(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateStopped || record.Generation != 6 {
		t.Fatalf("stopped record = %+v", record)
	}
	want := []string{
		"status:resolving sandbox", "resolve", "status:waiting for sandbox operation lock", "resolve",
		"status:checking existing runtime", "locate", "status:committing stopping state", "stopping",
		"status:stopping Cloud Hypervisor", "stop", "status:cleaning runtime state", "cleanup",
		"status:committing stopped state", "stopped", "report",
	}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestStopResumesStoppingAndRecoversStarting(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      types.SandboxState
		generation uint64
		want       uint64
	}{
		{name: "stopping", state: types.SandboxStateStopping, generation: 5, want: 6},
		{name: "starting", state: types.SandboxStateStarting, generation: 3, want: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, steps := newTestSandboxService(t, nil)
			if _, err := service.Create(t.Context(), CreateSandboxRequest{
				ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
			}); err != nil {
				t.Fatal(err)
			}
			catalog := service.lifecycle.(*fakeCatalog)
			catalog.record.State, catalog.record.Generation = test.state, test.generation
			service.runtime.(*fakeRuntime).observation = vmm.Observation{State: vmm.ProcessStarting, Process: vmm.Process{PID: 42}}
			*steps = nil
			record, err := service.Stop(t.Context(), "box")
			if err != nil {
				t.Fatal(err)
			}
			if record.State != types.SandboxStateStopped || record.Generation != test.want {
				t.Fatalf("stopped record = %+v", record)
			}
			if got := strings.Join(*steps, ","); strings.Contains(got, ",stopping,") || !strings.Contains(got, "locate,status:stopping Cloud Hypervisor,stop,status:cleaning runtime state,cleanup") {
				t.Fatalf("recovery steps = %v", *steps)
			}
		})
	}
}

func TestStopConvergesAbsentRunningWithoutSignalling(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.lifecycle.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	*steps = nil
	record, err := service.Stop(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateStopped || record.Generation != 5 {
		t.Fatalf("stopped record = %+v", record)
	}
	if got := strings.Join(*steps, ","); strings.Contains(got, ",stop,") || strings.Contains(got, ",stopping,") {
		t.Fatalf("absent VMM was signalled or marked Stopping: %v", *steps)
	}
}

func TestStopFailureRetainsRetryableStoppingState(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.lifecycle.(*fakeCatalog)
	catalog.record.State, catalog.record.Generation = types.SandboxStateRunning, 4
	failure := errors.New("signal failed")
	runtimeAdapter := service.runtime.(*fakeRuntime)
	runtimeAdapter.observation = vmm.Observation{State: vmm.ProcessRunning, Process: vmm.Process{PID: 42}}
	runtimeAdapter.stopErr = failure
	*steps = nil
	if _, err := service.Stop(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Stop error = %v", err)
	}
	if catalog.record.State != types.SandboxStateStopping || catalog.record.Generation != 5 {
		t.Fatalf("retained record = %+v", catalog.record)
	}
	if strings.Contains(strings.Join(*steps, ","), "cleanup") {
		t.Fatalf("runtime was cleaned before process absence: %v", *steps)
	}
}

func TestStopCreatedIsIdempotentAndPreservesCreated(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	created, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	})
	if err != nil {
		t.Fatal(err)
	}
	*steps = nil
	record, err := service.Stop(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateCreated || record.Generation != created.Generation {
		t.Fatalf("idempotent stop changed created record = %+v", record)
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got, "status:cleaning stale runtime state,cleanup,report") {
		t.Fatalf("idempotent steps = %v", *steps)
	}
}

func TestRemoveMarksDeletingBeforeDiskAndFinalizesAfterCleanup(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	*steps = nil
	record, err := service.Remove(t.Context(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != fixedID || record.State != types.SandboxStateDeleting || record.Generation != 3 {
		t.Fatalf("removed record = %+v", record)
	}
	want := []string{
		"status:resolving sandbox", "resolve", "status:waiting for sandbox operation lock",
		"status:marking sandbox for deletion", "deleting", "status:removing sandbox disk", "remove",
		"status:releasing metadata and image reference", "finalize", "report",
	}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestRemoveFailureRetainsDeletingAndRetryFinishes(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("disk cleanup failed")
	disks := service.cows.(fakeDisk)
	disks.remove = failure
	service.cows = disks
	*steps = nil
	if _, err := service.Remove(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Remove error = %v", err)
	} else {
		var classified *errdefs.Error
		if !errors.As(err, &classified) || !classified.Committed {
			t.Fatalf("Remove did not report committed Deleting state: %v", err)
		}
	}
	catalog := service.remover.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateDeleting || catalog.deleted {
		t.Fatalf("retained delete record = %+v, deleted=%v", catalog.record, catalog.deleted)
	}
	disks.remove = nil
	service.cows = disks
	*steps = nil
	if _, err := service.Remove(t.Context(), "box"); err != nil {
		t.Fatalf("retry Remove: %v", err)
	}
	if !catalog.deleted {
		t.Fatal("retry did not finalize metadata")
	}
	if got := *steps; !reflect.DeepEqual(got, []string{
		"status:resolving sandbox", "resolve", "status:waiting for sandbox operation lock",
		"status:marking sandbox for deletion", "deleting", "status:removing sandbox disk", "remove",
		"status:releasing metadata and image reference", "finalize", "report",
	}) {
		t.Fatalf("retry steps = %v", got)
	}
}

func TestRemoveRejectsRunningSandboxBeforeDiskCleanup(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.remover.(*fakeCatalog)
	catalog.record.State = types.SandboxStateRunning
	*steps = nil
	if _, err := service.Remove(t.Context(), "box"); err == nil {
		t.Fatal("Remove succeeded for a running sandbox")
	} else if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeStateConflict {
		t.Fatalf("Remove error code = %q, %v; want %q", code, err, errdefs.CodeStateConflict)
	}
	if catalog.record.State != types.SandboxStateRunning || catalog.deleted {
		t.Fatalf("running record changed = %+v, deleted=%v", catalog.record, catalog.deleted)
	}
	for _, step := range *steps {
		if step == "remove" || step == "finalize" {
			t.Fatalf("destructive step %q ran for a running sandbox: %v", step, *steps)
		}
	}
}
