package tap

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"

	filelock "github.com/kumabox/kumabox/internal/lock/flock"

	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/types"
)

// lease is both an address reservation and the recovery inventory. A hard link
// indexes it by sandbox/slot. Publishing the address first makes an interrupted
// allocation discoverable even before the sandbox index has been published.
type lease struct {
	Sandbox   types.SandboxID        `json:"sandbox"`
	Interface types.NetworkInterface `json:"interface"`
	// ReuseAfter is zero for active allocations. A retired reservation retains
	// no TAP or sandbox index, but prevents stale TCP tuples reaching a new VM.
	ReuseAfter time.Time `json:"reuse_after,omitzero"`
}

func (p *Provider) reserve(ctx context.Context, id types.SandboxID, spec network.AddSpec) (_ lease, returnErr error) {
	if _, err := types.ParseSandboxID(id.String()); err != nil {
		return lease{}, err
	}
	if spec.DHCP {
		return lease{}, errors.New("eBPF TAP networking requires fixed guest addressing")
	}
	if err := p.RecoverLeases(ctx); err != nil {
		return lease{}, err
	}
	var publishedPath string
	defer func() {
		if returnErr != nil && publishedPath != "" {
			// Link publication may succeed before syncing or indexing fails.
			// Keep that reservation discoverable by immediate compensation.
			item, err := p.readLease(publishedPath)
			if err == nil && item.Sandbox == id && item.Interface.Index == spec.Index && item.ReuseAfter.IsZero() {
				p.rememberLease(item)
			}
		}
	}()
	mac, ipv4, err := guestIdentity(spec.Index)
	if err != nil {
		return lease{}, err
	}
	name := p.tapName(id, spec.Index)
	if spec.Existing != nil {
		existing := spec.Existing
		if existing.Index != spec.Index || existing.TAP != name || existing.MAC != mac || existing.IPv4 == nil || *existing.IPv4 != *ipv4 || existing.AddressMode != types.AddressModeFixed || existing.Network != "ebpf" {
			return lease{}, errors.New("eBPF recovery must preserve the fixed network contract")
		}
	}
	queues := network.ResolveQueues(spec.Queues, 1)
	item := lease{Sandbox: id, Interface: types.NetworkInterface{
		Index: spec.Index, Name: "eth" + strconv.Itoa(spec.Index), TAP: name, MAC: mac,
		Queues: queues, QueueSize: network.DefaultQueueSize, Network: "ebpf",
		IPv4: ipv4, AddressMode: types.AddressModeFixed,
	}}
	indexPath := p.indexPath(id, spec.Index)
	if prior, err := p.readLease(indexPath); err == nil {
		if prior.Sandbox != id || prior.Interface.Index != spec.Index {
			return lease{}, errors.New("eBPF lease index ownership mismatch")
		}
		if spec.Existing != nil && prior.Interface.HostAddress != spec.Existing.HostAddress {
			return lease{}, errors.New("persisted eBPF host address differs from lease")
		}
		address, checkErr := p.readLease(filepath.Join(p.addressDir(), prior.Interface.HostAddress+".json"))
		if checkErr != nil {
			return lease{}, checkErr
		}
		if address.Sandbox != id || address.Interface.Index != spec.Index {
			return lease{}, errors.New("eBPF address ownership differs from sandbox index")
		}
		if !address.ReuseAfter.IsZero() {
			return lease{}, errors.New("sandbox index points to a retired eBPF address")
		}
		return prior, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return lease{}, err
	}
	if err := errors.Join(storage.EnsureDir(filepath.Dir(indexPath)), storage.EnsureDir(p.addressDir())); err != nil {
		return lease{}, err
	}
	base := binary.BigEndian.Uint32(p.pool.Addr().AsSlice())
	count := uint32(1) << (32 - p.pool.Bits())
	digest := sha256.Sum256([]byte(id.String() + "/" + strconv.Itoa(spec.Index)))
	start := binary.BigEndian.Uint32(digest[:4]) % (count - 2)
	for attempt := uint32(0); attempt < count-2; attempt++ {
		if err := ctx.Err(); err != nil {
			return lease{}, err
		}
		address := base + 1 + (start+attempt)%(count-2)
		var raw [4]byte
		binary.BigEndian.PutUint32(raw[:], address)
		item.Interface.HostAddress = netip.AddrFrom4(raw).String()
		if spec.Existing != nil {
			item.Interface = *spec.Existing
		}
		if err := item.Interface.Validate(); err != nil {
			return lease{}, err
		}
		host, err := netip.ParseAddr(item.Interface.HostAddress)
		if err != nil || !p.pool.Contains(host) {
			return lease{}, errors.New("recovery address is outside eBPF subnet")
		}
		addressPath := filepath.Join(p.addressDir(), item.Interface.HostAddress+".json")
		publishedPath = addressPath
		err = publishLease(addressPath, item)
		if errors.Is(err, os.ErrExist) {
			prior, readErr := p.readLease(addressPath)
			if readErr != nil {
				return lease{}, readErr
			}
			if !prior.ReuseAfter.IsZero() {
				if p.now().Before(prior.ReuseAfter) {
					if spec.Existing != nil {
						return lease{}, errors.New("persisted host address is still cooling down")
					}
					continue
				}
				if err := p.reclaimAddress(ctx, addressPath, item); errors.Is(err, os.ErrExist) {
					continue
				} else if err != nil {
					return lease{}, err
				}
			} else {
				if prior.Sandbox != id || prior.Interface.Index != spec.Index {
					if spec.Existing != nil {
						return lease{}, errors.New("recovery address is already reserved")
					}
					continue
				}
				item = prior
			}
		} else if err != nil {
			return lease{}, err
		}
		if err := os.Link(addressPath, indexPath); err != nil {
			return lease{}, err
		}
		return item, syncDirectory(filepath.Dir(indexPath))
	}
	return lease{}, errors.New("eBPF host address pool is exhausted")
}

// reclaimAddress serializes only reuse of one expired address. First-time
// allocations still use exclusive link publication without an IPAM-wide lock.
func (p *Provider) reclaimAddress(ctx context.Context, path string, item lease) (returnErr error) {
	guard := filelock.NewTransient(path + ".lock")
	if err := guard.Lock(ctx); err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, guard.Unlock(context.WithoutCancel(ctx))) }()
	prior, err := p.readLease(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if prior.ReuseAfter.IsZero() || p.now().Before(prior.ReuseAfter) {
			return os.ErrExist
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return publishLease(path, item)
}

func (p *Provider) retireAddress(item lease) (returnErr error) {
	path := filepath.Join(p.addressDir(), item.Interface.HostAddress+".json")
	prior, err := p.readLease(path)
	if err != nil {
		return err
	}
	if prior.Sandbox != item.Sandbox || prior.Interface.Index != item.Interface.Index {
		return errors.New("refuse to retire another endpoint owner's address")
	}
	item.ReuseAfter = p.now().UTC().Add(p.options.ReuseDelay)
	stage, err := os.CreateTemp(p.addressDir(), ".retire-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(stage.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	if err := json.NewEncoder(stage).Encode(item); err != nil {
		return errors.Join(err, stage.Close())
	}
	if err := stage.Close(); err != nil {
		return err
	}
	if err := storage.Publish(stage.Name(), path); err != nil {
		return err
	}
	p.forgetLease(item)
	return nil
}

// retireLease keeps the address discoverable if retirement fails after index
// removal. A retry in this same runtime must not need a whole-pool scan.
func (p *Provider) retireLease(item lease) error {
	p.rememberLease(item)
	path := p.indexPath(item.Sandbox, item.Interface.Index)
	if err := os.Remove(path); err == nil {
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return p.retireAddress(item)
}

func (p *Provider) addressDir() string { return filepath.Join(p.options.StateDir, "addresses") }

// indexedLeases keeps start/stop proportional to one sandbox's NIC count.
// Address-directory scans are reserved for orphan discovery and garbage collection.
func (p *Provider) indexedLeases(ctx context.Context, id types.SandboxID) ([]lease, error) {
	if _, err := types.ParseSandboxID(id.String()); err != nil {
		return nil, err
	}
	directory := filepath.Dir(p.indexPath(id, 0))
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]lease, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		item, err := p.readLease(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		if item.Sandbox != id {
			return nil, errors.New("eBPF indexed lease has another owner")
		}
		result = append(result, item)
	}
	return result, nil
}

func (p *Provider) indexPath(id types.SandboxID, index int) string {
	return filepath.Join(p.options.StateDir, "sandboxes", id.String(), strconv.Itoa(index)+".json")
}

func (p *Provider) readLease(path string) (lease, error) {
	if err := storage.CheckPath(path); err != nil {
		return lease{}, err
	}
	file, err := os.Open(path) //nolint:gosec // validated provider-owned path
	if err != nil {
		return lease{}, err
	}
	defer file.Close() //nolint:errcheck // read-only lease
	var item lease
	if err := json.NewDecoder(file).Decode(&item); err != nil {
		return lease{}, fmt.Errorf("read eBPF address reservation: %w", err)
	}
	if _, err := types.ParseSandboxID(item.Sandbox.String()); err != nil {
		return lease{}, err
	}
	if err := item.Interface.Validate(); err != nil {
		return lease{}, err
	}
	if item.Interface.AddressMode != types.AddressModeFixed || item.Interface.Network != "ebpf" {
		return lease{}, errors.New("invalid eBPF allocation contract")
	}
	if filepath.Dir(path) == p.addressDir() && filepath.Base(path) != item.Interface.HostAddress+".json" {
		return lease{}, errors.New("eBPF reservation filename differs from its address")
	}
	return item, nil
}

// publishLease uses link rather than rename: two concurrent allocations must
// never overwrite an existing address. The reservation is complete before it
// becomes visible, and survives a crash before index publication.
func publishLease(path string, item lease) (returnErr error) {
	stage, err := os.CreateTemp(filepath.Dir(path), ".lease-")
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, os.Remove(stage.Name())) }()
	if err := json.NewEncoder(stage).Encode(item); err != nil {
		return errors.Join(err, stage.Close())
	}
	if err := errors.Join(stage.Sync(), stage.Close()); err != nil {
		return err
	}
	if err := os.Link(stage.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // provider-owned directory
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// scanLeases includes reservations whose sandbox index was never published.
// Global discovery belongs to node startup and GC, not ordinary removal.
func (p *Provider) scanLeases(ctx context.Context) ([]lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p.addressDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []lease
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		item, err := p.readLease(filepath.Join(p.addressDir(), entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			// Reuse replaces an expired quarantine entry under its own lock.
			// A global discovery pass can overlap that brief unlink window.
			continue
		}
		if err != nil {
			return nil, err
		}
		if !item.ReuseAfter.IsZero() {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}
