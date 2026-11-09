package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/internal/disk"
	"github.com/kumabox/kumabox/internal/errdefs"
	filelock "github.com/kumabox/kumabox/internal/lock/flock"
	"github.com/kumabox/kumabox/internal/types"
	"github.com/kumabox/kumabox/internal/vmm"
)

type fakeCloneDisk struct {
	fakeDisk
	source       string
	clonedData   []types.DataDiskSpec
	preparedData []types.DataDiskSpec
}

type forceRemovalRuntime struct {
	*fakeRuntime
	beforeStop func(vmm.Process)
	cleanupErr error
}

func (r *forceRemovalRuntime) Stop(ctx context.Context, process vmm.Process, options types.StopOptions) error {
	if r.beforeStop != nil {
		r.beforeStop(process)
	}
	if err := r.fakeRuntime.Stop(ctx, process, options); err != nil {
		return err
	}
	r.observation = vmm.Observation{State: vmm.ProcessAbsent}
	return nil
}

func (r *forceRemovalRuntime) Cleanup(ctx context.Context, id types.SandboxID) error {
	if r.observation.State != vmm.ProcessAbsent {
		return errors.New("owned VMM is still present")
	}
	return errors.Join(r.fakeRuntime.Cleanup(ctx, id), r.cleanupErr)
}

type forceRemovalDisk struct {
	disk.Backend
	check func(types.SandboxID)
}

func (d *forceRemovalDisk) Remove(ctx context.Context, id types.SandboxID) error {
	d.check(id)
	return d.Backend.Remove(ctx, id)
}

type removalCatalog struct {
	sandboxCatalog
	references []string
}

func (c *removalCatalog) Resolve(ctx context.Context, reference string) (types.Sandbox, error) {
	c.references = append(c.references, reference)
	return c.sandboxCatalog.Resolve(ctx, reference)
}

func (d *fakeCloneDisk) Clone(_ context.Context, _ types.SandboxID, _ int64, source string) error {
	*d.steps = append(*d.steps, "disk-clone")
	d.source = source
	return nil
}

func (d *fakeCloneDisk) CloneData(_ context.Context, _ types.SandboxID, specs []types.DataDiskSpec, _ string) error {
	d.clonedData = append([]types.DataDiskSpec(nil), specs...)
	return nil
}

func (d *fakeCloneDisk) PrepareData(_ context.Context, _ types.SandboxID, specs []types.DataDiskSpec) error {
	d.preparedData = append([]types.DataDiskSpec(nil), specs...)
	return nil
}

func (d *fakeCloneDisk) CheckData(context.Context, types.SandboxID, []types.DataDiskSpec) error {
	return nil
}

func TestCreateFromSnapshotUsesDiskCloneWithoutFormatting(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	disk := &fakeCloneDisk{fakeDisk: fakeDisk{steps: steps}}
	service.dependencies.disks = disk
	_, err := service.createFromSnapshot(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config:         types.SandboxConfig{Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}, cloneDiskOrigin{cowPath: "/snapshots/checkpoint/cow.raw", dataDirectory: "/snapshots/checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	if disk.source != "/snapshots/checkpoint/cow.raw" {
		t.Fatalf("disk clone source = %q", disk.source)
	}
	for _, step := range *steps {
		if step == "disk" {
			t.Fatalf("clone formatted a disposable disk: %v", *steps)
		}
	}
}

func TestCreateCloneSeparatesInheritedAndNewDataDisks(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	backend := &fakeCloneDisk{fakeDisk: fakeDisk{steps: steps}}
	service.dependencies.disks = backend
	specs := []types.DataDiskSpec{{Name: "db", Size: types.MinDataDiskSize, FSType: "ext4"}, {Name: "logs", Size: types.MinDataDiskSize, FSType: "none"}}
	_, err := service.createFromSnapshot(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{
			Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, DataDisks: specs,
		},
	}, cloneDiskOrigin{cowPath: "/snapshot/cow.raw", dataDirectory: "/snapshot", inheritedDataCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.clonedData) != 1 || backend.clonedData[0].Name != "db" ||
		len(backend.preparedData) != 1 || backend.preparedData[0].Name != "logs" {
		t.Fatalf("clone split = inherited %+v, new %+v", backend.clonedData, backend.preparedData)
	}
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
	if record.VMM != types.VMMCloudHypervisor {
		t.Fatalf("VMM = %q, want %q", record.VMM, types.VMMCloudHypervisor)
	}
	want := []string{"status:resolving and checking image", "verify", "reserve", "status:creating sparse ext4 disk", "disk", "status:committing created state", "created", "report"}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestCreatePublishesResolvedNetworkWithCreatedState(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	record, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 2, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.State != types.SandboxStateCreated || record.Config.NetworkName != "default" ||
		record.Network.Backend != types.NetworkBackendCNI || len(record.Network.Interfaces) != 2 {
		t.Fatalf("created network record = %+v", record)
	}
	networks := testNetwork(t, service)
	if len(networks.specs) != 2 || networks.specs[0].Queues != 4 || networks.specs[1].Queues != 4 {
		t.Fatalf("network specs = %+v", networks.specs)
	}
	want := []string{
		"status:resolving and checking image", "verify", "reserve",
		"status:preparing sandbox network", "network-prepare",
		"status:allocating sandbox network interfaces", "network-add",
		"status:creating sparse ext4 disk", "disk",
		"status:committing created state", "created", "report",
	}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestCreateNetworkFailureCleansResourcesBeforeForgettingReservation(t *testing.T) {
	failure := errors.New("CNI add failed")
	service, steps := newTestSandboxService(t, nil)
	networks := testNetwork(t, service)
	networks.addErr = failure
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 1,
		},
	}); !errors.Is(err, failure) {
		t.Fatalf("Create error = %v", err)
	}
	wantTail := []string{"network-add", "remove", "network-delete", "forget"}
	if got := (*steps)[len(*steps)-len(wantTail):]; !reflect.DeepEqual(got, wantTail) {
		t.Fatalf("cleanup steps = %v, want %v", got, wantTail)
	}
}

func TestCreateRetainsNetworkOwnerWhenCleanupFails(t *testing.T) {
	addFailure := errors.New("CNI add failed")
	deleteFailure := errors.New("CNI delete failed")
	service, steps := newTestSandboxService(t, nil)
	networks := testNetwork(t, service)
	networks.addErr, networks.deleteErr = addFailure, deleteFailure
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 1,
		},
	}); !errors.Is(err, addFailure) || !errors.Is(err, deleteFailure) {
		t.Fatalf("Create error = %v", err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateError || catalog.record.Failure == nil || catalog.record.Failure.Phase != "network add" {
		t.Fatalf("retained record = %+v", catalog.record)
	}
	wantTail := []string{"network-add", "remove", "network-delete", "error"}
	if got := (*steps)[len(*steps)-len(wantTail):]; !reflect.DeepEqual(got, wantTail) {
		t.Fatalf("cleanup steps = %v, want %v", got, wantTail)
	}
}

func TestCreateRejectsUnavailableVMMBeforeReservation(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	_, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config:         types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
		VMM:            types.VMMFirecracker,
	})
	if err == nil {
		t.Fatal("Create accepted an unavailable VMM")
	}
	if code, ok := errdefs.CodeOf(err); !ok || code != errdefs.CodeHostIncompatible {
		t.Fatalf("Create error = %v", err)
	}
	if len(*steps) != 0 {
		t.Fatalf("Create mutated state before rejecting VMM: %v", *steps)
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
	guard := service.dependencies.images.(fakeGuard)
	guard.afterUse = failure
	service.dependencies.images = guard
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
	disks := service.dependencies.disks.(fakeDisk)
	disks.remove = removeFailure
	service.dependencies.disks = disks
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); !errors.Is(err, prepareFailure) || !errors.Is(err, removeFailure) {
		t.Fatalf("Create error = %v", err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateError || catalog.record.Failure == nil || catalog.record.Failure.Phase != "disk" {
		t.Fatalf("retained record = %+v", catalog.record)
	}
	wantTail := []string{"disk", "remove", "error"}
	if got := (*steps)[len(*steps)-len(wantTail):]; !reflect.DeepEqual(got, wantTail) {
		t.Fatalf("cleanup steps = %v, want %v", got, wantTail)
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
		"resolve",
		"status:marking sandbox for deletion", "deleting", "status:removing sandbox disk", "remove",
		"status:removing VMM logs", "remove-logs",
		"status:releasing metadata and image reference", "finalize", "report",
	}
	if !reflect.DeepEqual(*steps, want) {
		t.Fatalf("steps = %v, want %v", *steps, want)
	}
}

func TestForceRemovalKeepsIdentityLockedThroughStopAndCleanup(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	base := service.dependencies.catalog.(*fakeCatalog)
	base.record = types.Sandbox{
		ID: fixedID, VMM: types.VMMCloudHypervisor,
		Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateRunning, Generation: 4,
	}
	catalog := &removalCatalog{sandboxCatalog: base}
	service.dependencies.catalog = catalog
	runtime := &forceRemovalRuntime{fakeRuntime: testRuntime(t, service)}
	process := vmm.Process{SandboxID: fixedID, Generation: 3, PID: 42}
	runtime.observation = vmm.Observation{State: vmm.ProcessRunning, Process: process}
	assertLocked := func() {
		t.Helper()
		path, err := service.dependencies.paths.Lock(fixedID)
		if err != nil {
			t.Fatal(err)
		}
		probe := filelock.New(path)
		locked, err := probe.TryLock(t.Context())
		if locked {
			_ = probe.Unlock(t.Context())
		}
		if err != nil || locked {
			t.Fatalf("removal released the identity lock: acquired=%t, error=%v", locked, err)
		}
	}
	runtime.beforeStop = func(got vmm.Process) {
		assertLocked()
		if got != process || base.record.State != types.SandboxStateStopping {
			t.Fatalf("termination ownership = %+v, %+v", got, base.record)
		}
	}
	var err error
	service.dependencies.runtimes, err = vmm.NewRegistry(runtime)
	if err != nil {
		t.Fatal(err)
	}
	service.dependencies.disks = &forceRemovalDisk{Backend: service.dependencies.disks, check: func(id types.SandboxID) {
		assertLocked()
		if id != fixedID || runtime.observation.State != vmm.ProcessAbsent {
			t.Fatal("disk removal began before the owned VMM exited")
		}
	}}
	removed, err := service.RemoveWithOptions(t.Context(), "box", types.RemoveOptions{Force: true})
	if err != nil || removed.ID != fixedID || !base.deleted || !runtime.stopOptions.Force {
		t.Fatalf("forced removal = %+v, %v", removed, err)
	}
	if !slices.Equal(catalog.references, []string{"box", fixedID.String()}) {
		t.Fatalf("removal re-resolved a reusable name: %v", catalog.references)
	}
	if stop, remove := slices.Index(*steps, "stop"), slices.Index(*steps, "remove"); stop < 0 || remove <= stop {
		t.Fatalf("stop/delete ordering = %v", *steps)
	}
}

func TestForceRemovalHandlesEveryStoppableState(t *testing.T) {
	for _, test := range []struct {
		state      types.SandboxState
		generation uint64
		live       bool
	}{
		{state: types.SandboxStateRunning, generation: 4, live: true},
		{state: types.SandboxStateStarting, generation: 3, live: true},
		{state: types.SandboxStateStopping, generation: 5, live: true},
		{state: types.SandboxStateError, generation: 4, live: true},
		{state: types.SandboxStateRunning, generation: 4},
		{state: types.SandboxStateCreating, generation: 1},
		{state: types.SandboxStateCreated, generation: 2},
		{state: types.SandboxStateStopped, generation: 6},
		{state: types.SandboxStateDeleting, generation: 7},
	} {
		t.Run(string(test.state)+"/live="+fmt.Sprint(test.live), func(t *testing.T) {
			service, steps := newTestSandboxService(t, nil)
			catalog := service.dependencies.catalog.(*fakeCatalog)
			catalog.record = types.Sandbox{
				ID: fixedID, VMM: types.VMMCloudHypervisor,
				Config: types.SandboxConfig{Name: "box"}, State: test.state, Generation: test.generation,
			}
			runtime := &forceRemovalRuntime{fakeRuntime: testRuntime(t, service)}
			if test.live {
				generation, err := stopProcessGeneration(catalog.record)
				if err != nil {
					t.Fatal(err)
				}
				runtime.observation = vmm.Observation{
					State:   vmm.ProcessRunning,
					Process: vmm.Process{SandboxID: fixedID, Generation: generation, PID: 42},
				}
			}
			var err error
			service.dependencies.runtimes, err = vmm.NewRegistry(runtime)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.RemoveWithOptions(t.Context(), "box", types.RemoveOptions{Force: true}); err != nil {
				t.Fatal(err)
			}
			if !catalog.deleted || slices.Contains(*steps, "stop") != test.live || test.live && !runtime.stopOptions.Force {
				t.Fatalf("removal = %+v, steps = %v", catalog.record, *steps)
			}
		})
	}
}

func TestForceRemovalRetriesCanceledStopBeforeTouchingDisks(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record = types.Sandbox{
		ID: fixedID, VMM: types.VMMCloudHypervisor,
		Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateRunning, Generation: 4,
	}
	runtime := &forceRemovalRuntime{fakeRuntime: testRuntime(t, service)}
	runtime.observation = vmm.Observation{
		State:   vmm.ProcessRunning,
		Process: vmm.Process{SandboxID: fixedID, Generation: 3, PID: 42},
	}
	runtime.stopErr = context.Canceled
	var err error
	service.dependencies.runtimes, err = vmm.NewRegistry(runtime)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := service.RemoveWithOptions(t.Context(), "box", types.RemoveOptions{Force: true})
	var classified *errdefs.Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &classified) || !classified.Committed ||
		retained.State != types.SandboxStateStopping || catalog.deleted || slices.Contains(*steps, "remove") {
		t.Fatalf("canceled removal = %+v, %v, steps = %v", retained, err, *steps)
	}
	runtime.stopErr = nil
	if _, err := service.RemoveWithOptions(t.Context(), "box", types.RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if !catalog.deleted {
		t.Fatal("retry retained the sandbox after successful termination")
	}
}

func TestForceRemovalRetainsDeletingAndChecksRuntimeOnRetry(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	catalog := service.dependencies.catalog.(*fakeCatalog)
	catalog.record = types.Sandbox{
		ID: fixedID, VMM: types.VMMCloudHypervisor,
		Config: types.SandboxConfig{Name: "box"}, State: types.SandboxStateStopped, Generation: 6,
	}
	runtime := &forceRemovalRuntime{fakeRuntime: testRuntime(t, service)}
	var err error
	service.dependencies.runtimes, err = vmm.NewRegistry(runtime)
	if err != nil {
		t.Fatal(err)
	}
	disks := service.dependencies.disks.(fakeDisk)
	failure := errors.New("disk removal failed")
	disks.remove = failure
	service.dependencies.disks = disks
	if _, err := service.RemoveWithOptions(t.Context(), "box", types.RemoveOptions{Force: true}); !errors.Is(err, failure) {
		t.Fatalf("first removal = %v", err)
	}
	if catalog.deleted || catalog.record.State != types.SandboxStateDeleting {
		t.Fatalf("failed deletion lost ownership: %+v", catalog.record)
	}
	*steps = nil
	runtime.cleanupErr = errors.New("runtime cleanup still busy")
	if _, err := service.RemoveWithOptions(t.Context(), "box", types.RemoveOptions{Force: true}); !errors.Is(err, runtime.cleanupErr) {
		t.Fatalf("unsafe retry = %v", err)
	}
	if slices.Contains(*steps, "remove") || catalog.deleted {
		t.Fatal("retry deleted disks without proving process absence")
	}
	runtime.cleanupErr, disks.remove = nil, nil
	service.dependencies.disks = disks
	if _, err := service.RemoveWithOptions(t.Context(), "box", types.RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if !catalog.deleted {
		t.Fatal("retry did not finish deletion")
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
	disks := service.dependencies.disks.(fakeDisk)
	disks.remove = failure
	service.dependencies.disks = disks
	*steps = nil
	if _, err := service.Remove(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Remove error = %v", err)
	} else {
		var classified *errdefs.Error
		if !errors.As(err, &classified) || !classified.Committed {
			t.Fatalf("Remove did not report committed Deleting state: %v", err)
		}
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateDeleting || catalog.deleted {
		t.Fatalf("retained delete record = %+v, deleted=%v", catalog.record, catalog.deleted)
	}
	disks.remove = nil
	service.dependencies.disks = disks
	*steps = nil
	if _, err := service.Remove(t.Context(), "box"); err != nil {
		t.Fatalf("retry Remove: %v", err)
	}
	if !catalog.deleted {
		t.Fatal("retry did not finalize metadata")
	}
	if got := *steps; !reflect.DeepEqual(got, []string{
		"status:resolving sandbox", "resolve", "status:waiting for sandbox operation lock",
		"resolve",
		"status:marking sandbox for deletion", "deleting", "status:removing sandbox disk", "remove",
		"status:removing VMM logs", "remove-logs",
		"status:releasing metadata and image reference", "finalize", "report",
	}) {
		t.Fatalf("retry steps = %v", got)
	}
}

func TestRemoveNetworkFailureRetainsDeletingUntilRetry(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo",
		Config: types.SandboxConfig{
			Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory,
			Storage: types.DefaultSandboxStorage, NICs: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("network cleanup failed")
	networks := testNetwork(t, service)
	networks.deleteErr = failure
	*steps = nil
	if _, err := service.Remove(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Remove error = %v", err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateDeleting || catalog.deleted {
		t.Fatalf("retained delete record = %+v, deleted=%v", catalog.record, catalog.deleted)
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got, "remove,status:removing sandbox network,network-delete") || strings.Contains(got, "finalize") {
		t.Fatalf("network cleanup ordering = %v", *steps)
	}

	networks.deleteErr = nil
	*steps = nil
	if _, err := service.Remove(t.Context(), "box"); err != nil {
		t.Fatal(err)
	}
	if !catalog.deleted {
		t.Fatal("retry did not finalize metadata")
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got, "remove,status:removing sandbox network,network-delete") || !strings.Contains(got, "finalize") {
		t.Fatalf("retry did not repeat idempotent cleanup: %v", *steps)
	}
}

func TestRemoveLogFailureRetainsDeletingUntilRetry(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("log cleanup failed")
	runtimeAdapter := testRuntime(t, service)
	runtimeAdapter.removeLogsErr = failure
	*steps = nil
	if _, err := service.Remove(t.Context(), "box"); !errors.Is(err, failure) {
		t.Fatalf("Remove error = %v", err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateDeleting || catalog.deleted {
		t.Fatalf("retained delete record = %+v, deleted=%v", catalog.record, catalog.deleted)
	}
	if got := strings.Join(*steps, ","); strings.Contains(got, "finalize") || !strings.Contains(got, "remove,status:removing VMM logs,remove-logs") {
		t.Fatalf("log cleanup ordering = %v", *steps)
	}

	runtimeAdapter.removeLogsErr = nil
	*steps = nil
	if _, err := service.Remove(t.Context(), "box"); err != nil {
		t.Fatal(err)
	}
	if !catalog.deleted {
		t.Fatal("retry did not finalize metadata")
	}
	if got := strings.Join(*steps, ","); !strings.Contains(got, "remove,status:removing VMM logs,remove-logs") || !strings.Contains(got, "finalize") {
		t.Fatalf("retry did not repeat idempotent cleanup: %v", *steps)
	}
}

func TestRemoveRejectsRunningSandboxBeforeDiskCleanup(t *testing.T) {
	service, steps := newTestSandboxService(t, nil)
	if _, err := service.Create(t.Context(), CreateSandboxRequest{
		ImageReference: "demo", Config: types.SandboxConfig{Name: "box", CPUs: 1, Memory: types.DefaultSandboxMemory, Storage: types.DefaultSandboxStorage},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := service.dependencies.catalog.(*fakeCatalog)
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
