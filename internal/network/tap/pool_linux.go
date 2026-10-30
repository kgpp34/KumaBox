//go:build linux

package tap

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/storage"
	"github.com/kumabox/kumabox/internal/types"
)

type poolPhase uint8

const (
	poolPreparing poolPhase = iota
	poolReady
	poolReserved
	poolActive
	poolCleaning
)

// tapShape includes the guest slot: its gateway and MAC differ between slots.
// Queues is the total RX/TX count, not just the multi-queue flag.
type tapShape struct{ slot, queues int }

type pooledTAP struct {
	name     string
	shape    tapShape
	epoch    uint64
	owner    types.SandboxID
	phase    poolPhase
	prepared bool
	lastErr  error
}

// tapPool owns idle host devices, never VMs. The mutex protects only identity
// and allocation decisions; netlink, BPF and filesystem work happen outside it.
//
//	Preparing -> Ready -> Reserved -> Active -> Cleaning -> Ready
//	                                             |          |
//	                                             +-- retry  +-- trim
//
// Active/reserved ownership is durable in address leases. Idle identity and
// epoch survive daemon exit in the kernel alias. Reboot discards idle devices;
// active leases still reserve their exact names for lifecycle recovery.
type tapPool struct {
	startMu sync.Mutex
	mu      sync.Mutex
	entries map[string]*pooledTAP
	ready   map[tapShape][]*pooledTAP
	cancel  context.CancelFunc
	done    chan struct{}
	wake    chan struct{}
	closed  bool
}

func (p *Provider) poolAlias(shape tapShape, epoch uint64) string {
	return fmt.Sprintf("kumabox:%s:pool:%d:%d:%d", p.owner, shape.slot, shape.queues, epoch)
}

func (p *Provider) parsePoolAlias(alias string) (tapShape, uint64, bool) {
	parts := strings.Split(alias, ":")
	if len(parts) != 6 || parts[0] != "kumabox" || parts[1] != p.owner || parts[2] != "pool" {
		return tapShape{}, 0, false
	}
	slot, a := strconv.Atoi(parts[3])
	queues, b := strconv.Atoi(parts[4])
	epoch, c := strconv.ParseUint(parts[5], 10, 64)
	if a != nil || b != nil || c != nil || slot < 0 || slot >= types.MaxSandboxNICs || queues < 2 || queues%2 != 0 {
		return tapShape{}, 0, false
	}
	return tapShape{slot, queues}, epoch, true
}

func (s *tapPool) init() {
	if s.entries == nil {
		s.entries = make(map[string]*pooledTAP)
		s.ready = make(map[tapShape][]*pooledTAP)
		s.wake = make(chan struct{}, 1)
	}
}

func (s *tapPool) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// StartPool recovers live ownership before prebuilding idle devices. Its caller
// owns the context for the provider lifetime; Close joins the refill worker.
// Zero PoolSize preserves the existing lazy, non-pooled backend behavior.
func (p *Provider) StartPool(ctx context.Context) error {
	if p.options.PoolSize == 0 {
		if _, err := os.Lstat(p.poolMarker()); errors.Is(err, os.ErrNotExist) {
			return ctx.Err()
		} else if err != nil {
			return err
		}
	} else if err := p.ensurePoolMarker(); err != nil {
		return err
	}
	p.taps.startMu.Lock()
	defer p.taps.startMu.Unlock()
	p.taps.mu.Lock()
	p.taps.init()
	closed, started := p.taps.closed, p.taps.cancel != nil
	p.taps.mu.Unlock()
	if closed {
		return os.ErrClosed
	}
	if started {
		return ctx.Err()
	}
	items, err := p.scanLeases(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := p.adoptPooledLease(item); err != nil {
			return err
		}
	}
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	unowned := false
	for _, link := range links {
		shape, epoch, owned := p.parsePoolAlias(link.Attrs().Alias)
		if !owned || link.Type() != "tuntap" {
			continue
		}
		p.taps.mu.Lock()
		if p.taps.entries[link.Attrs().Name] == nil {
			p.taps.entries[link.Attrs().Name] = &pooledTAP{name: link.Attrs().Name, shape: shape, epoch: epoch, phase: poolCleaning}
			unowned = true
		}
		p.taps.mu.Unlock()
	}
	if p.options.PoolSize == 0 && !unowned {
		return ctx.Err() // Active leases stay owned; no idle devices need cleanup.
	}
	d, release, err := p.dataPlane(ctx)
	if err != nil {
		return err
	}
	defer release()
	p.maintainPool(ctx, d, true)
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.options.PoolSize == 0 {
		p.taps.mu.Lock()
		empty := len(p.taps.entries) == 0
		p.taps.mu.Unlock()
		if empty {
			return os.Remove(p.poolMarker())
		}
		// Busy unowned devices remain quarantined. Keep a cleanup worker even
		// with refilling disabled, so turning pooling off does not leak TAPs.
	}
	p.taps.mu.Lock()
	var initErr error
	if p.options.PoolSize > 0 && p.idleCountLocked() == 0 {
		for _, entry := range p.taps.entries {
			initErr = errors.Join(initErr, entry.lastErr)
		}
	}
	p.taps.mu.Unlock()
	if initErr != nil {
		return fmt.Errorf("prepare idle TAP pool: %w", initErr)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	p.taps.mu.Lock()
	p.taps.cancel, p.taps.done = cancel, make(chan struct{})
	p.taps.mu.Unlock()
	go p.runPool(workerCtx)
	return nil
}

func (p *Provider) poolMarker() string { return filepath.Join(p.options.StateDir, "tap-pool-v1") }

// The marker is synced once before any pooled device is created. It lets a
// later disabled configuration reclaim idle devices without probing host
// networking for installations that have never used the pool.
func (p *Provider) ensurePoolMarker() error {
	if err := storage.EnsureDir(p.options.StateDir); err != nil {
		return err
	}
	path := p.poolMarker()
	if err := storage.CheckPath(path); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // provider-owned validated marker
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = file.WriteString("1\n")
	if err := errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	return syncDirectory(p.options.StateDir)
}

func (p *Provider) runPool(ctx context.Context) {
	defer close(p.taps.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.taps.wake:
		case <-ticker.C:
		}
		d, release, err := p.dataPlane(ctx)
		if err != nil {
			return
		}
		p.maintainPool(ctx, d, false)
		release()
	}
}

func (s *tapPool) close() {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.mu.Lock()
	s.closed = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (p *Provider) newPooledTAP(shape tapShape) (*pooledTAP, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	identity := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random[:]))
	name := p.options.TAPPrefix + "p" + identity[:14-len(p.options.TAPPrefix)]
	entry := &pooledTAP{name: name, shape: shape, phase: poolPreparing}
	p.taps.mu.Lock()
	defer p.taps.mu.Unlock()
	p.taps.init()
	if p.taps.closed {
		return nil, os.ErrClosed
	}
	if _, exists := p.taps.entries[name]; exists {
		return nil, errors.New("pooled TAP name collision")
	}
	p.taps.entries[name] = entry
	return entry, nil
}

func (p *Provider) adoptPooledLease(item lease) error {
	if item.Epoch == 0 {
		return nil
	}
	p.taps.mu.Lock()
	defer p.taps.mu.Unlock()
	p.taps.init()
	entry := p.taps.entries[item.Interface.TAP]
	shape := tapShape{item.Interface.Index, item.Interface.Queues}
	if entry == nil {
		p.taps.entries[item.Interface.TAP] = &pooledTAP{name: item.Interface.TAP, shape: shape, epoch: item.Epoch, owner: item.Sandbox, phase: poolActive}
		return nil
	}
	if entry.owner != item.Sandbox || entry.epoch != item.Epoch || entry.shape != shape {
		return errors.New("pooled TAP lease conflicts with another lifecycle")
	}
	return nil
}

func (p *Provider) borrowTAP(ctx context.Context, shape tapShape, id types.SandboxID, d *datapath) (*pooledTAP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.taps.mu.Lock()
	p.taps.init()
	if p.taps.closed {
		p.taps.mu.Unlock()
		return nil, os.ErrClosed
	}
	var entry *pooledTAP
	if free := p.taps.ready[shape]; len(free) > 0 {
		entry = free[len(free)-1]
		p.taps.ready[shape] = free[:len(free)-1]
	}
	p.taps.mu.Unlock()
	if entry == nil {
		var err error
		entry, err = p.newPooledTAP(shape)
		if err != nil {
			return nil, err
		}
		if err := p.preparePoolLink(entry, d); err != nil {
			p.poolFailure(entry, err)
			return nil, err
		}
	}
	p.taps.mu.Lock()
	defer p.taps.mu.Unlock()
	if entry.epoch == math.MaxUint64 {
		entry.phase = poolCleaning
		return nil, errors.New("pooled TAP epoch exhausted")
	}
	entry.epoch++
	entry.owner, entry.phase = id, poolReserved
	if len(p.taps.ready[shape]) <= p.options.PoolSize/2 {
		p.taps.signal()
	}
	return entry, nil
}

func (p *Provider) allocateLease(ctx context.Context, id types.SandboxID, spec network.AddSpec, d *datapath) (lease, error) {
	var borrowed *pooledTAP
	var bind func() (tapBinding, error)
	if p.options.PoolSize > 0 {
		bind = func() (tapBinding, error) {
			var err error
			borrowed, err = p.borrowTAP(ctx, tapShape{spec.Index, network.ResolveQueues(spec.Queues, 1)}, id, d)
			if err != nil {
				return tapBinding{}, err
			}
			return tapBinding{borrowed.name, borrowed.epoch}, nil
		}
	}
	item, err := p.reserveWith(ctx, id, spec, bind)
	if err == nil {
		err = p.adoptPooledLease(item)
	} else if borrowed != nil {
		// Failed durable publication can still leave a valid reservation. Only
		// return unowned devices to maintenance; GC owns published lifecycles.
		leases, scanErr := p.leases(context.WithoutCancel(ctx), id)
		if scanErr == nil {
			published := false
			for _, saved := range leases {
				published = published || saved.Interface.TAP == borrowed.name
			}
			if !published {
				p.poolFailure(borrowed, err)
			}
		}
	}
	return item, err
}

func (p *Provider) poolFailure(entry *pooledTAP, err error) {
	p.taps.mu.Lock()
	entry.phase, entry.owner, entry.lastErr = poolCleaning, "", err
	p.taps.mu.Unlock()
}

func (p *Provider) idleCountLocked() int {
	count := 0
	for _, entries := range p.taps.ready {
		count += len(entries)
	}
	return count
}

func (p *Provider) markPoolReady(entry *pooledTAP) error {
	p.taps.mu.Lock()
	keep := !p.taps.closed && p.idleCountLocked() < p.options.PoolSize*len(p.options.PoolQueues) && entry.epoch < math.MaxUint64
	if keep {
		entry.phase, entry.owner, entry.lastErr = poolReady, "", nil
		p.taps.ready[entry.shape] = append(p.taps.ready[entry.shape], entry)
	}
	p.taps.mu.Unlock()
	if keep {
		return nil
	}
	link, err := netlink.LinkByName(entry.name)
	var missing netlink.LinkNotFoundError
	if err == nil {
		shape, epoch, owned := p.parsePoolAlias(link.Attrs().Alias)
		if !owned || shape != entry.shape || epoch != entry.epoch {
			err = errors.New("refuse trimming unrelated or newer TAP")
		} else if err = requireIdleQueues(link); err == nil {
			err = netlink.LinkDel(link)
		}
	}
	if err != nil && !errors.As(err, &missing) {
		p.poolFailure(entry, err)
		return err
	}
	p.taps.mu.Lock()
	delete(p.taps.entries, entry.name)
	p.taps.mu.Unlock()
	return nil
}

func (p *Provider) maintainPool(ctx context.Context, d *datapath, initial bool) {
	p.taps.mu.Lock()
	var cleaning []*pooledTAP
	for _, entry := range p.taps.entries {
		if entry.phase == poolCleaning && entry.owner == "" {
			entry.phase = poolPreparing
			cleaning = append(cleaning, entry)
		}
	}
	p.taps.mu.Unlock()
	for _, entry := range cleaning {
		if err := ctx.Err(); err != nil {
			p.poolFailure(entry, err)
			continue
		}
		if err := p.cleanPoolLink(entry, d, ""); err != nil {
			var missing netlink.LinkNotFoundError
			if errors.As(err, &missing) {
				p.taps.mu.Lock()
				delete(p.taps.entries, entry.name)
				p.taps.mu.Unlock()
				continue
			}
			p.poolFailure(entry, err)
			continue
		}
		if err := p.preparePoolLink(entry, d); err != nil {
			p.poolFailure(entry, err)
			continue
		}
		_ = p.markPoolReady(entry) // failure remains fenced for the next pass
	}
	for _, queues := range p.options.PoolQueues {
		shape := tapShape{0, queues}
		p.taps.mu.Lock()
		refill := initial || len(p.taps.ready[shape]) <= p.options.PoolSize/2
		for _, entry := range p.taps.entries {
			if entry.shape == shape && entry.phase == poolCleaning && entry.owner == "" {
				refill = false // Retry existing failed resources before creating more.
			}
		}
		p.taps.mu.Unlock()
		if !refill {
			continue
		}
		for ctx.Err() == nil {
			p.taps.mu.Lock()
			full := len(p.taps.ready[shape]) >= p.options.PoolSize || p.idleCountLocked() >= p.options.PoolSize*len(p.options.PoolQueues)
			p.taps.mu.Unlock()
			if full {
				break
			}
			entry, err := p.newPooledTAP(shape)
			if err != nil {
				break
			}
			if err := p.preparePoolLink(entry, d); err != nil {
				p.poolFailure(entry, err)
				break
			}
			if err := p.markPoolReady(entry); err != nil {
				break
			}
		}
	}
}

func (p *Provider) releasePooledTAP(ctx context.Context, item lease, d *datapath) error {
	if err := p.adoptPooledLease(item); err != nil {
		return err
	}
	p.taps.mu.Lock()
	entry := p.taps.entries[item.Interface.TAP]
	entry.phase = poolCleaning
	p.taps.mu.Unlock()
	err := p.cleanPoolLink(entry, d, item.Interface.HostAddress)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		err = clearConntrack(item.Interface.HostAddress)
	}
	if err != nil {
		return err // The durable lease and owner remain fenced for Delete/GC retry.
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.retireLease(item); err != nil {
		return err
	}
	if err := p.readyPoolLink(entry, d); err != nil {
		p.poolFailure(entry, err)
		return err
	}
	return p.markPoolReady(entry)
}
