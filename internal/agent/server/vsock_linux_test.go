//go:build linux

package server

import (
	"errors"
	"io"
	"testing"
	"time"
)

func TestServeVsockRetriesListenerFailure(t *testing.T) {
	originalAttempt := serveVsockAttempt
	originalWait := waitVsockRetry
	defer func() {
		serveVsockAttempt = originalAttempt
		waitVsockRetry = originalWait
	}()

	attempts := 0
	serveVsockAttempt = func(uint32, func(io.ReadWriter)) error {
		attempts++
		if attempts < 3 {
			return errors.New("restored listener is stale")
		}
		return nil
	}
	var delays []time.Duration
	waitVsockRetry = func(delay time.Duration) {
		delays = append(delays, delay)
	}

	if err := serveVsock(Port, func(io.ReadWriter) {}); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("listener attempts = %d, want 3", attempts)
	}
	if len(delays) != 2 || delays[0] != vsockListenRetryInterval || delays[1] != vsockListenRetryInterval {
		t.Fatalf("retry delays = %v", delays)
	}
}
