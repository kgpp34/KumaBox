package core

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kumabox/kumabox/errdefs"
	"github.com/kumabox/kumabox/types"
)

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
