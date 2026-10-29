package cloudhypervisor

import "context"

// apiGate limits one monitor's control queue without coupling different VMs.
// References include waiters, so entries can be removed immediately when idle.
type apiGate struct {
	token chan struct{}
	users int
}

func (d *Driver) acquireAPI(ctx context.Context, socket string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.apiMu.Lock()
	if d.apiCalls == nil {
		d.apiCalls = make(map[string]*apiGate)
	}
	gate := d.apiCalls[socket]
	if gate == nil {
		gate = &apiGate{token: make(chan struct{}, 1)}
		d.apiCalls[socket] = gate
	}
	gate.users++
	d.apiMu.Unlock()
	forget := func() {
		d.apiMu.Lock()
		defer d.apiMu.Unlock()
		gate.users--
		if gate.users == 0 {
			delete(d.apiCalls, socket)
		}
	}
	select {
	case gate.token <- struct{}{}:
		return func() { <-gate.token; forget() }, nil
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}
