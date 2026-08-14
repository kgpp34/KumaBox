package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kumabox/kumabox/internal/backend"
	"github.com/kumabox/kumabox/internal/config"
	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/state"
	"github.com/kumabox/kumabox/internal/vm"
)

func TestStartVMRecoversPersistedNetworks(t *testing.T) {
	for _, metadataBackend := range []string{"json", "sqlite"} {
		t.Run(metadataBackend, func(t *testing.T) {
			rt, rec := newNetworkRecoveryRuntime(t, metadataBackend, 2)
			withVerifyNetworkConfig(t, func(kbnetwork.Config) error {
				return fmt.Errorf("%w: test network is missing", kbnetwork.ErrNetworkUnavailable)
			})

			var requests []kbnetwork.CNIAddRequest
			withAddCNI(t, func(
				_ context.Context,
				_ string,
				_ config.NetworkConfig,
				req kbnetwork.CNIAddRequest,
			) (*kbnetwork.Allocation, error) {
				requests = append(requests, req)
				return allocationForExisting(t, rec.ID, req), nil
			})

			started, err := rt.StartVMContext(t.Context(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if started.State != vm.StateRunning {
				t.Fatalf("state = %s, want running", started.State)
			}
			if len(requests) != 2 {
				t.Fatalf("CNI recovery requests = %d, want 2", len(requests))
			}
			for index, req := range requests {
				if req.Existing == nil {
					t.Fatalf("request %d has no persisted network config", index)
				}
				want := rec.NetworkConfigs[index]
				if req.Existing.TAP != want.TAP || req.Existing.MAC != want.MAC ||
					req.Existing.Network.IP != want.Network.IP {
					t.Fatalf("request %d identity = %+v, want %+v", index, req.Existing, want)
				}
			}
			records, err := rt.data.Networks.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 2 {
				t.Fatalf("provider records = %d, want 2", len(records))
			}
		})
	}
}

func TestStartVMRollsBackPartialNetworkRecovery(t *testing.T) {
	rt, rec := newNetworkRecoveryRuntime(t, "json", 2)
	withVerifyNetworkConfig(t, func(kbnetwork.Config) error {
		return fmt.Errorf("%w: test network is missing", kbnetwork.ErrNetworkUnavailable)
	})
	recoveryErr := errors.New("second CNI recovery failed")
	withAddCNI(t, func(
		_ context.Context,
		_ string,
		_ config.NetworkConfig,
		req kbnetwork.CNIAddRequest,
	) (*kbnetwork.Allocation, error) {
		if req.Index == 1 {
			return nil, recoveryErr
		}
		return allocationForExisting(t, rec.ID, req), nil
	})
	var deleted []kbnetwork.CNIDeleteRequest
	withDeleteCNI(t, func(
		_ context.Context,
		_ string,
		_ config.NetworkConfig,
		req kbnetwork.CNIDeleteRequest,
	) error {
		deleted = append(deleted, req)
		return nil
	})

	_, err := rt.StartVMContext(t.Context(), rec.ID)
	if !errors.Is(err, recoveryErr) {
		t.Fatalf("start error = %v, want %v", err, recoveryErr)
	}
	if len(deleted) != 1 || deleted[0].IfName != "eth0" || !deleted[0].PreserveNetNS {
		t.Fatalf("rollback deletes = %+v", deleted)
	}
	records, err := rt.data.Networks.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("provider records after rollback = %+v", records)
	}
}

func TestStartVMRepairsMissingNetworkProviderRecord(t *testing.T) {
	for _, metadataBackend := range []string{"json", "sqlite"} {
		t.Run(metadataBackend, func(t *testing.T) {
			rt, rec := newNetworkRecoveryRuntime(t, metadataBackend, 1)
			withVerifyNetworkConfig(t, func(kbnetwork.Config) error { return nil })
			withAddCNI(t, func(
				context.Context,
				string,
				config.NetworkConfig,
				kbnetwork.CNIAddRequest,
			) (*kbnetwork.Allocation, error) {
				t.Fatal("healthy network must not be recreated")
				return nil, nil
			})

			if _, err := rt.StartVMContext(t.Context(), rec.ID); err != nil {
				t.Fatal(err)
			}
			records, err := rt.data.Networks.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].ID != rec.NetworkConfigs[0].ID || records[0].VMID != rec.ID {
				t.Fatalf("repaired provider records = %+v", records)
			}
		})
	}
}

func TestConcurrentStartRecoversNetworkOnce(t *testing.T) {
	rt, rec := newNetworkRecoveryRuntime(t, "json", 1)
	var healthy atomic.Bool
	withVerifyNetworkConfig(t, func(kbnetwork.Config) error {
		if healthy.Load() {
			return nil
		}
		return fmt.Errorf("%w: test network is missing", kbnetwork.ErrNetworkUnavailable)
	})
	var recoveryCalls atomic.Int32
	withAddCNI(t, func(
		_ context.Context,
		_ string,
		_ config.NetworkConfig,
		req kbnetwork.CNIAddRequest,
	) (*kbnetwork.Allocation, error) {
		recoveryCalls.Add(1)
		healthy.Store(true)
		return allocationForExisting(t, rec.ID, req), nil
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := rt.StartVMContext(t.Context(), rec.ID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := recoveryCalls.Load(); got != 1 {
		t.Fatalf("network recovery calls = %d, want 1", got)
	}
}

func newNetworkRecoveryRuntime(t *testing.T, metadataBackend string, interfaceCount int) (*Runtime, *vm.VMRecord) {
	t.Helper()
	rootDir := filepath.Join(t.TempDir(), "data")
	cfg := testRuntimeConfig(rootDir)
	cfg.Metadata.Backend = metadataBackend
	if metadataBackend == "sqlite" {
		cfg.Metadata.Path = filepath.Join(rootDir, "metadata", "kumabox.db")
		if err := state.InitSQLiteMetadata(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	stores, err := state.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stores.Metadata != nil {
		t.Cleanup(func() {
			if err := stores.Metadata.Close(); err != nil {
				t.Errorf("close metadata: %v", err)
			}
		})
	}
	var nextPID atomic.Int32
	rt, err := NewWithBackendAndState(stores, backendFake{
		render: func(*vm.VMRecord) error { return nil },
		start: func(*vm.VMRecord) (*backend.StartResult, error) {
			pid := nextPID.Add(1)
			return &backend.StartResult{PID: int(pid), APISocket: fmt.Sprintf("/tmp/ch-%d.sock", pid)}, nil
		},
		observe: func(*vm.VMRecord) vm.Observation {
			return vm.Observation{State: vm.ObservedStateRunning, CheckedAt: time.Now().UTC()}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.cfg = cfg
	rec, err := stores.VM.Create(vm.CreateRequest{
		Name: "network-recovery", RootDisk: "base.qcow2", Kernel: "vmlinuz", Initrd: "initrd.img",
		Network: "multi", RunDir: filepath.Join(rootDir, "run"), LogDir: filepath.Join(rootDir, "log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	configs := make([]kbnetwork.Config, 0, interfaceCount)
	for index := range interfaceCount {
		configs = append(configs, recoveryNetworkConfig(rec.ID, index))
	}
	rec, err = stores.VM.SetNetworkConfigs(rec.ID, configs)
	if err != nil {
		t.Fatal(err)
	}
	return rt, rec
}

func recoveryNetworkConfig(vmID string, index int) kbnetwork.Config {
	return kbnetwork.Config{
		ID: kbnetwork.NetworkID(vmID, index), NetworkName: fmt.Sprintf("cni:net%d", index),
		TAP: fmt.Sprintf("kbtap%d", index), MAC: fmt.Sprintf("5a:00:00:00:00:%02x", index+1),
		NumQueues: 2, QueueSize: 512, Backend: kbnetwork.ProviderCNI,
		IfName: fmt.Sprintf("eth%d", index), NetnsPath: kbnetwork.NetNSPath(vmID),
		Network: &kbnetwork.GuestInfo{
			IP: fmt.Sprintf("10.90.0.%d", index+2), Gateway: "10.90.0.1", Prefix: 24,
		},
	}
}

func allocationForExisting(t *testing.T, vmID string, req kbnetwork.CNIAddRequest) *kbnetwork.Allocation {
	t.Helper()
	if req.Existing == nil {
		t.Fatal("recovery request is missing Existing config")
	}
	now := time.Now().UTC()
	return &kbnetwork.Allocation{
		Config: *req.Existing,
		Record: networkRecordFromConfig(vmID, *req.Existing, now),
	}
}

func withVerifyNetworkConfig(t *testing.T, fn func(kbnetwork.Config) error) {
	t.Helper()
	previous := verifyNetworkConfig
	verifyNetworkConfig = fn
	t.Cleanup(func() {
		verifyNetworkConfig = previous
	})
}
