package core

import (
	"context"
	"errors"
	"testing"

	filelock "github.com/kumabox/kumabox/lock/flock"
	"github.com/kumabox/kumabox/types"
)

type switchingSandboxCatalog struct {
	sandboxCatalog
	reads  int
	second types.Sandbox
	err    error
}

func (c *switchingSandboxCatalog) Resolve(ctx context.Context, reference string) (types.Sandbox, error) {
	c.reads++
	if c.reads == 2 {
		return c.second, c.err
	}
	return c.sandboxCatalog.Resolve(ctx, reference)
}

func TestLockExistingSandboxUsesRecordReadUnderLock(t *testing.T) {
	service, _ := newTestSandboxService(t, nil)
	base := service.dependencies.catalog.(*fakeCatalog)
	base.record = types.Sandbox{ID: fixedID, Generation: 2}
	updated := base.record
	updated.Generation = 3
	catalog := &switchingSandboxCatalog{sandboxCatalog: base, second: updated}
	service.dependencies.catalog = catalog

	record, unlock, err := service.lockExistingSandbox(t.Context(), sandboxLockRequest{reference: "box", operation: "test", retryHint: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	if record.Generation != 3 || catalog.reads != 2 {
		t.Fatalf("locked record = %+v after %d reads", record, catalog.reads)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestLockExistingSandboxReleasesOnSecondResolveFailure(t *testing.T) {
	service, _ := newTestSandboxService(t, nil)
	base := service.dependencies.catalog.(*fakeCatalog)
	base.record = types.Sandbox{ID: fixedID, Generation: 2}
	catalog := &switchingSandboxCatalog{sandboxCatalog: base, err: errors.New("record disappeared")}
	service.dependencies.catalog = catalog

	_, unlock, err := service.lockExistingSandbox(t.Context(), sandboxLockRequest{reference: "box", operation: "test", retryHint: "retry"})
	if err == nil || unlock != nil {
		t.Fatalf("second resolve returned unlock %v and error %v", unlock != nil, err)
	}
	path, err := service.dependencies.paths.Lock(fixedID)
	if err != nil {
		t.Fatal(err)
	}
	probe := filelock.New(path)
	if locked, err := probe.TryLock(t.Context()); err != nil || !locked {
		t.Fatalf("lock retained after failed resolve: locked=%t, error=%v", locked, err)
	}
	if err := probe.Unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
}
