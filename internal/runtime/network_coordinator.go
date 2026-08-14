package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	kbnetwork "github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/vm"
)

// networkCoordinator owns host-side network allocation, provider state, and
// rollback. It embeds Runtime only to share the already-injected stores and
// configuration; VM lifecycle code reaches network operations through this
// boundary instead of implementing provider details itself.
type networkCoordinator struct {
	*Runtime
}

func (r *Runtime) initNetworkCoordinator() {
	r.network = &networkCoordinator{Runtime: r}
	r.disk = &storageCoordinator{Runtime: r}
}

func (r *networkCoordinator) providerStore() (*kbnetwork.Store, error) {
	store, ok := r.data.Networks.(*kbnetwork.Store)
	if !ok {
		return nil, fmt.Errorf("network provider operations require a concrete network store")
	}
	return store, nil
}

type recoveredNetwork struct {
	config       kbnetwork.Config
	previous     *kbnetwork.Record
	hostRefAdded bool
}

const networkRollbackTimeout = 30 * time.Second

func (r *networkCoordinator) ensureNetwork(ctx context.Context, rec *vm.VMRecord) error {
	if rec == nil || len(rec.NetworkConfigs) == 0 {
		return nil
	}
	records, err := r.data.Networks.List()
	if err != nil {
		return fmt.Errorf("list network provider records: %w", err)
	}
	providerRecords := make(map[string]kbnetwork.Record, len(records))
	for _, record := range records {
		providerRecords[record.ID] = record
	}

	recovered := make([]recoveredNetwork, 0, len(rec.NetworkConfigs))
	for index := range rec.NetworkConfigs {
		persisted := rec.NetworkConfigs[index]
		if err := verifyNetworkConfig(persisted); err == nil {
			if err := r.repairNetworkRecord(rec.ID, persisted, providerRecords[persisted.ID]); err != nil {
				return r.networkRecoveryError(ctx, rec, persisted, err, recovered)
			}
			continue
		} else if !errors.Is(err, kbnetwork.ErrNetworkUnavailable) {
			return r.networkRecoveryError(ctx, rec, persisted,
				fmt.Errorf("verify VM network: %w", err), recovered)
		}

		selection := networkSelectionForConfig(rec, persisted)
		allocation, hostRefAdded, err := r.attachNetworkConfigWithExisting(ctx, rec, selection, index, &persisted)
		if err != nil {
			return r.networkRecoveryError(ctx, rec, persisted, err, recovered)
		}
		if err := validateRecoveredNetwork(persisted, allocation.Config); err != nil {
			current := recoveredNetwork{config: allocation.Config, hostRefAdded: hostRefAdded}
			if previous, ok := providerRecords[persisted.ID]; ok {
				current.previous = &previous
			}
			recovered = append(recovered, current)
			return r.networkRecoveryError(ctx, rec, persisted, err, recovered)
		}
		current := recoveredNetwork{config: allocation.Config, hostRefAdded: hostRefAdded}
		if previous, ok := providerRecords[persisted.ID]; ok {
			current.previous = &previous
		}
		recovered = append(recovered, current)
	}
	return nil
}

func (r *networkCoordinator) repairNetworkRecord(
	vmID string,
	config kbnetwork.Config,
	existing kbnetwork.Record,
) error {
	if existing.ID != "" && existing.VMID != vmID {
		return fmt.Errorf("%w: network record %s belongs to VM %s", kbnetwork.ErrNetworkConflict, config.ID, existing.VMID)
	}
	if existing.ID != "" && networkRecordMatchesConfig(existing, config) {
		return nil
	}
	now := time.Now().UTC()
	record := networkRecordFromConfig(vmID, config, now)
	if existing.ID != "" {
		record.CreatedAt = existing.CreatedAt
		record.Cleanup = existing.Cleanup
	}
	if err := r.data.Networks.UpsertRecord(record); err != nil {
		return fmt.Errorf("repair network provider record %s: %w", config.ID, err)
	}
	return nil
}

func (r *networkCoordinator) networkRecoveryError(
	ctx context.Context,
	rec *vm.VMRecord,
	config kbnetwork.Config,
	recoveryErr error,
	recovered []recoveredNetwork,
) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), networkRollbackTimeout)
	defer cancel()
	rollbackErr := r.rollbackRecoveredNetworks(rollbackCtx, rec, recovered)
	if rollbackErr != nil {
		recoveryErr = errors.Join(recoveryErr, fmt.Errorf("rollback recovered networks: %w", rollbackErr))
	}
	return fmt.Errorf("recover VM %s network %s: %w", rec.Name, config.ID, recoveryErr)
}

func (r *networkCoordinator) rollbackRecoveredNetworks(
	ctx context.Context,
	rec *vm.VMRecord,
	recovered []recoveredNetwork,
) error {
	var rollbackErrs []error
	for i := len(recovered) - 1; i >= 0; i-- {
		item := recovered[i]
		if item.config.Backend == kbnetwork.ProviderCNI {
			err := deleteCNI(ctx, r.cfg.Runtime.RootDir, r.cfg.Network, kbnetwork.CNIDeleteRequest{
				VMID:          rec.ID,
				Network:       networkSelectionForConfig(rec, item.config),
				IfName:        cniIfName(item.config),
				TAP:           item.config.TAP,
				NetNSPath:     item.config.NetnsPath,
				PreserveNetNS: true,
			})
			if err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		} else if err := deleteHostTap(item.config.TAP); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
		if err := r.data.Networks.DeleteRecord(item.config.ID); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
		if item.hostRefAdded {
			if err := r.data.Networks.DecrementHostTapRef(1); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		if item.previous != nil {
			if err := r.data.Networks.UpsertRecord(*item.previous); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
	}
	return errors.Join(rollbackErrs...)
}

func validateRecoveredNetwork(want, got kbnetwork.Config) error {
	if want.ID != got.ID || want.Backend != got.Backend || want.TAP != got.TAP ||
		!strings.EqualFold(want.MAC, got.MAC) || want.IfName != got.IfName ||
		want.NetnsPath != got.NetnsPath || want.NetworkName != got.NetworkName {
		return fmt.Errorf("%w: recovered network identity changed: want=%+v got=%+v",
			kbnetwork.ErrNetworkConflict, want, got)
	}
	if want.Network == nil && got.Network == nil {
		return nil
	}
	if want.Network == nil || got.Network == nil || want.Network.IP != got.Network.IP ||
		want.Network.Prefix != got.Network.Prefix || want.Network.Gateway != got.Network.Gateway {
		return fmt.Errorf("%w: recovered guest network changed: want=%+v got=%+v",
			kbnetwork.ErrNetworkConflict, want.Network, got.Network)
	}
	return nil
}

func networkRecordMatchesConfig(record kbnetwork.Record, config kbnetwork.Config) bool {
	want := networkRecordFromConfig(record.VMID, config, record.UpdatedAt)
	return record.ID == want.ID && record.Network == want.Network && record.Provider == want.Provider &&
		record.IfName == want.IfName && record.TAP == want.TAP && strings.EqualFold(record.MAC, want.MAC) &&
		record.NumQueues == want.NumQueues && record.QueueSize == want.QueueSize &&
		record.BridgeDev == want.BridgeDev && record.NetnsPath == want.NetnsPath &&
		strings.Join(record.IPs, ",") == strings.Join(want.IPs, ",") && record.Gateway == want.Gateway &&
		strings.Join(record.DNS, ",") == strings.Join(want.DNS, ",")
}

func networkRecordFromConfig(vmID string, networkConfig kbnetwork.Config, now time.Time) kbnetwork.Record {
	record := kbnetwork.Record{
		ID: networkConfig.ID, VMID: vmID, Network: networkConfig.NetworkName,
		Provider: networkConfig.Backend, IfName: networkConfig.IfName, TAP: networkConfig.TAP,
		MAC: networkConfig.MAC, NumQueues: networkConfig.NumQueues, QueueSize: networkConfig.QueueSize,
		BridgeDev: networkConfig.BridgeDev, NetnsPath: networkConfig.NetnsPath,
		CreatedAt: now, UpdatedAt: now,
	}
	if networkConfig.Network != nil {
		if networkConfig.Network.IP != "" {
			record.IPs = []string{fmt.Sprintf("%s/%d", networkConfig.Network.IP, networkConfig.Network.Prefix)}
		}
		record.Gateway = networkConfig.Network.Gateway
		record.DNS = append([]string(nil), networkConfig.Network.DNS...)
	}
	return record
}
