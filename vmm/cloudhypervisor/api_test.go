package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPIQueueIsPerMonitorAndCancelable(t *testing.T) {
	driver := &Driver{}
	release, err := driver.acquireAPI(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := driver.acquireAPI(ctx, "first"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter: %v", err)
	}
	other, err := driver.acquireAPI(t.Context(), "second")
	if err != nil {
		t.Fatal(err)
	}
	other()
	waiting, stop := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer stop()
	if _, err := driver.acquireAPI(waiting, "first"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter deadline: %v", err)
	}
	release()
	if len(driver.apiCalls) != 0 {
		t.Fatal("idle monitor entries were retained")
	}
}

func TestConcurrentQueriesDoNotOverflowMonitorAPI(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "kb-ch-api-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	socket := filepath.Join(directory, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Int32
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if active.Add(1) != 1 {
			t.Error("concurrent requests reached one monitor")
		}
		defer active.Add(-1)
		time.Sleep(time.Millisecond)
		_, _ = fmt.Fprint(w, `{"state":"Running"}`)
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	driver := &Driver{}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = driver.queryInfo(t.Context(), socket)
			} else {
				err = driver.snapshotAction(t.Context(), socket, "vm.pause", nil, time.Second)
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(driver.apiCalls) != 0 {
		t.Fatal("query queue was not released")
	}
}
