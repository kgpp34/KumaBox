package tap

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/kumabox/kumabox/internal/types"
)

// leaseRecovery bridges address-first publication and its sandbox hard-link
// index. Historical quarantine records are scanned at startup/GC, never for
// each sandbox deletion. Only unindexed candidates need an in-memory fallback;
// every candidate is checked against current durable ownership before use.
type leaseRecovery struct {
	ready atomic.Bool
	gate  chan struct{}
	mu    sync.Mutex
	byID  map[types.SandboxID]map[string]lease
}

// RecoverLeases discovers interrupted publications before node readiness.
// Callers own the state root for this Provider's lifetime. A failed or canceled
// scan is retryable; a new Provider repeats recovery after a runtime restart.
// OwnedIDs performs fresh global discovery during GC.
func (p *Provider) RecoverLeases(ctx context.Context) error {
	if p.recovery.ready.Load() {
		return ctx.Err()
	}
	select {
	case p.recovery.gate <- struct{}{}:
		defer func() { <-p.recovery.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.recovery.ready.Load() {
		return ctx.Err()
	}
	items, err := p.scanLeases(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		p.rememberLease(item)
	}
	p.recovery.ready.Store(true)
	return nil
}

func (p *Provider) rememberLease(item lease) {
	p.recovery.mu.Lock()
	defer p.recovery.mu.Unlock()
	if p.recovery.byID == nil {
		p.recovery.byID = make(map[types.SandboxID]map[string]lease)
	}
	items := p.recovery.byID[item.Sandbox]
	if items == nil {
		items = make(map[string]lease)
		p.recovery.byID[item.Sandbox] = items
	}
	items[item.Interface.HostAddress] = item
}

func (p *Provider) forgetLease(item lease) {
	p.recovery.mu.Lock()
	defer p.recovery.mu.Unlock()
	items := p.recovery.byID[item.Sandbox]
	if saved, found := items[item.Interface.HostAddress]; found && saved.Interface.Index == item.Interface.Index {
		delete(items, item.Interface.HostAddress)
		if len(items) == 0 {
			delete(p.recovery.byID, item.Sandbox)
		}
	}
}

func (p *Provider) recoveryLeases(id types.SandboxID) []lease {
	p.recovery.mu.Lock()
	defer p.recovery.mu.Unlock()
	items := p.recovery.byID[id]
	result := make([]lease, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	return result
}

// leases is proportional to this sandbox's NICs and interrupted publications.
// A cache entry is only a discovery hint: a retired or reassigned address must
// never authorize cleanup of its current owner's TAP or conntrack state.
func (p *Provider) leases(ctx context.Context, id types.SandboxID) ([]lease, error) {
	if _, err := types.ParseSandboxID(id.String()); err != nil {
		return nil, err
	}
	if err := p.RecoverLeases(ctx); err != nil {
		return nil, err
	}
	indexed, err := p.indexedLeases(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]lease, 0, len(indexed))
	seen := make(map[string]bool, len(indexed))
	for _, item := range indexed {
		current, err := p.readLease(filepath.Join(p.addressDir(), item.Interface.HostAddress+".json"))
		if err != nil {
			return nil, err
		}
		if current.Sandbox != id || current.Interface.Index != item.Interface.Index || !current.ReuseAfter.IsZero() {
			return nil, errors.New("eBPF indexed address ownership mismatch")
		}
		seen[current.Interface.HostAddress] = true
		result = append(result, current)
		p.forgetLease(current) // The durable sandbox index now carries recovery.
	}
	for _, item := range p.recoveryLeases(id) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seen[item.Interface.HostAddress] {
			continue
		}
		current, err := p.readLease(filepath.Join(p.addressDir(), item.Interface.HostAddress+".json"))
		if errors.Is(err, os.ErrNotExist) || (err == nil && (current.Sandbox != id || current.Interface.Index != item.Interface.Index || !current.ReuseAfter.IsZero())) {
			p.forgetLease(item)
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, current)
	}
	slices.SortFunc(result, func(a, b lease) int {
		if order := cmp.Compare(a.Interface.Index, b.Interface.Index); order != 0 {
			return order
		}
		return cmp.Compare(a.Interface.HostAddress, b.Interface.HostAddress)
	})
	return result, nil
}
