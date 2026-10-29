package tap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/types"
)

func TestIndexedLeaseLookupDoesNotReadUnrelatedHistory(t *testing.T) {
	p := testProvider(t)
	id, _ := types.NewSandboxID()
	item, err := p.reserve(t.Context(), id, network.AddSpec{})
	if err != nil {
		t.Fatal(err)
	}
	// Existing providers must not rescan unrelated records after recovery.
	// Fresh global recovery must still report corruption rather than hide it.
	corrupt := filepath.Join(p.addressDir(), "broken.json")
	if err := os.WriteFile(corrupt, []byte("invalid lease"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := p.leases(t.Context(), id)
	if err != nil || len(items) != 1 || items[0].Interface.HostAddress != item.Interface.HostAddress {
		t.Fatalf("target lookup read unrelated history: %+v %v", items, err)
	}
	reopened, err := New(p.options)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecoverLeases(t.Context()); err == nil {
		t.Fatal("startup recovery hid corrupt inventory")
	}
	if err := os.Remove(corrupt); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecoverLeases(t.Context()); err != nil {
		t.Fatalf("failed recovery could not retry: %v", err)
	}
}

func TestLeaseRecoveryFindsUnindexedPublicationAfterRestart(t *testing.T) {
	p := testProvider(t)
	id, _ := types.NewSandboxID()
	item, err := p.reserve(t.Context(), id, network.AddSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.indexPath(id, 0)); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(p.options)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := reopened.RecoverLeases(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled recovery: %v", err)
	}
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			items, err := reopened.leases(t.Context(), id)
			if err != nil || len(items) != 1 || items[0].Interface.HostAddress != item.Interface.HostAddress {
				t.Errorf("lost interrupted publication: %+v %v", items, err)
			}
		}()
	}
	workers.Wait()
}

func TestFailedIndexPublicationCanBeCompensatedInSameRuntime(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory write denial used for fault injection")
	}
	p := testProvider(t)
	id, _ := types.NewSandboxID()
	indexDir := filepath.Dir(p.indexPath(id, 0))
	for _, directory := range []string{indexDir, p.addressDir()} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(indexDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(indexDir, 0o700) })
	if _, err := p.reserve(t.Context(), id, network.AddSpec{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("index publication did not fail as intended: %v", err)
	}
	if err := os.Chmod(indexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	items, err := p.leases(t.Context(), id)
	if err != nil || len(items) != 1 {
		t.Fatalf("same-runtime compensation lost its unindexed reservation: %+v %v", items, err)
	}
}

func TestRetirementFailureRemainsDiscoverableWithoutIndex(t *testing.T) {
	p := testProvider(t)
	id, _ := types.NewSandboxID()
	item, err := p.reserve(t.Context(), id, network.AddSpec{})
	if err != nil {
		t.Fatal(err)
	}
	address := filepath.Join(p.addressDir(), item.Interface.HostAddress+".json")
	data, err := os.ReadFile(address)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(address, []byte("unreadable retirement record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.retireLease(item); err == nil {
		t.Fatal("retirement failure was hidden")
	}
	if _, err := os.Stat(p.indexPath(id, 0)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test did not reach retirement after index removal: %v", err)
	}
	if err := os.WriteFile(address, data, 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := p.leases(t.Context(), id)
	if err != nil || len(items) != 1 || items[0].Interface.HostAddress != item.Interface.HostAddress {
		t.Fatalf("retry lost ownership: %+v %v", items, err)
	}
	if err := p.retireLease(items[0]); err != nil {
		t.Fatal(err)
	}
	items, err = p.leases(t.Context(), id)
	if err != nil || len(items) != 0 {
		t.Fatalf("retired address still active: %+v %v", items, err)
	}
}

func TestRecoveryHintCannotAuthorizeAnotherAddressOwner(t *testing.T) {
	p := testProvider(t)
	id, _ := types.NewSandboxID()
	item, err := p.reserve(t.Context(), id, network.AddSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.indexPath(id, 0)); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(p.options)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecoverLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	newOwner, _ := types.NewSandboxID()
	replacement := item
	replacement.Sandbox = newOwner
	replacement.Interface.TAP = p.tapName(newOwner, 0)
	data, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.addressDir(), item.Interface.HostAddress+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := reopened.leases(t.Context(), id)
	if err != nil || len(items) != 0 {
		t.Fatalf("stale hint exposed another owner's address: %+v %v", items, err)
	}
}
