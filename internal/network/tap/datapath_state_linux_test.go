//go:build linux

package tap

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDatapathStateSharesConcurrentOpen(t *testing.T) {
	var state datapathState
	t.Cleanup(func() { _ = state.close() })
	want := &datapath{}
	var opens atomic.Int32
	started, proceed := make(chan struct{}, 32), make(chan struct{})
	open := func(context.Context) (*datapath, error) {
		opens.Add(1)
		started <- struct{}{}
		<-proceed
		return want, nil
	}
	var callers sync.WaitGroup
	for range 32 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			d, release, err := state.acquire(t.Context(), open)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			if d != want {
				t.Error("concurrent callers received different descriptors")
			}
		}()
	}
	<-started
	close(proceed)
	callers.Wait()
	if opens.Load() != 1 {
		t.Fatalf("opened %d descriptor sets for one provider", opens.Load())
	}
}

func TestDatapathStateRetriesInitializationAndCancelsWaiters(t *testing.T) {
	var state datapathState
	t.Cleanup(func() { _ = state.close() })
	failure := errors.New("bpffs unavailable")
	if _, _, err := state.acquire(t.Context(), func(context.Context) (*datapath, error) {
		return nil, failure
	}); !errors.Is(err, failure) {
		t.Fatalf("initialization failure = %v", err)
	}
	started, proceed, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, release, err := state.acquire(t.Context(), func(context.Context) (*datapath, error) {
			close(started)
			<-proceed
			return &datapath{}, nil
		})
		if err == nil {
			release()
		}
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, _, err := state.acquire(ctx, func(context.Context) (*datapath, error) {
		t.Error("waiter opened a second descriptor set")
		return nil, failure
	})
	close(proceed)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("canceled waiter = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("retrying initializer = %v", err)
	}
	d, release, err := state.acquire(t.Context(), func(context.Context) (*datapath, error) {
		t.Error("successful initialization was not cached")
		return nil, failure
	})
	if err != nil || d == nil {
		t.Fatalf("acquire after canceled waiter = %v", err)
	}
	release()
	canceled, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	if _, _, err := state.acquire(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cached acquisition = %v", err)
	}
}

func TestDatapathStateCloseDrainsBorrowers(t *testing.T) {
	var state datapathState
	_, release, err := state.acquire(t.Context(), func(context.Context) (*datapath, error) {
		return &datapath{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- state.close() }()
	select {
	case err := <-done:
		release()
		t.Fatalf("closed while a borrower still holds descriptors: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not drain the released borrower")
	}
	if _, _, err := state.acquire(t.Context(), nil); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("acquire after shutdown = %v", err)
	}
	if err := state.close(); err != nil {
		t.Fatalf("repeated close = %v", err)
	}
}

func TestDatapathStateCloseWaitsForInitializer(t *testing.T) {
	var state datapathState
	started, proceed := make(chan struct{}), make(chan struct{})
	acquired, closed := make(chan error, 1), make(chan error, 1)
	go func() {
		_, release, err := state.acquire(t.Context(), func(context.Context) (*datapath, error) {
			close(started)
			<-proceed
			return &datapath{}, nil
		})
		if err == nil {
			release()
		}
		acquired <- err
	}()
	<-started
	go func() { closed <- state.close() }()
	// The initializer holds no read lock. Wait until shutdown has fenced new
	// borrowers, then let it finish creating its descriptors.
	deadline := time.After(2 * time.Second)
	for {
		state.mu.RLock()
		closing := state.closed
		state.mu.RUnlock()
		if closing {
			break
		}
		select {
		case <-deadline:
			close(proceed)
			t.Fatal("shutdown did not fence new borrowers")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case err := <-closed:
		close(proceed)
		t.Fatalf("close returned before initializer released descriptors: %v", err)
	default:
	}
	close(proceed)
	if err := <-acquired; !errors.Is(err, os.ErrClosed) {
		t.Errorf("initialization racing shutdown = %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if state.current != nil {
		t.Fatal("shutdown retained newly opened descriptors")
	}
}
