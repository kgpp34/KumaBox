package core

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kumabox/kumabox/sandbox"
	"github.com/kumabox/kumabox/storage"
	"github.com/kumabox/kumabox/types"
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
	steps  *[]string
	record types.Sandbox
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

func (f *fakeCatalog) Forget(context.Context, types.SandboxID, uint64) error {
	*f.steps = append(*f.steps, "forget")
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

func newTestSandboxService(t *testing.T, diskError error) (*SandboxService, *[]string) {
	t.Helper()
	digest, err := types.ParseDigest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	paths, err := sandbox.NewPaths(storage.Roots{
		Data: filepath.Join(base, "data"), Run: filepath.Join(base, "run"), Log: filepath.Join(base, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	steps := []string{}
	catalog := &fakeCatalog{steps: &steps}
	service := newSandboxService(paths, fakeGuard{image: types.Image{ManifestDigest: digest}, steps: &steps}, catalog, fakeDisk{steps: &steps, prepare: diskError}, fakeReporter{steps: &steps})
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
	catalog := service.catalog.(*fakeCatalog)
	if catalog.record.State != types.SandboxStateError || catalog.record.Failure == nil || catalog.record.Failure.Phase != "disk" {
		t.Fatalf("retained record = %+v", catalog.record)
	}
	wantTail := []string{"disk", "remove", "error"}
	if got := (*steps)[len(*steps)-len(wantTail):]; !reflect.DeepEqual(got, wantTail) {
		t.Fatalf("cleanup steps = %v, want %v", got, wantTail)
	}
}
