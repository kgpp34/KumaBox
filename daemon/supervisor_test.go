package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSnapshotUsageFlushRetriesAndStopsWithNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var diagnostics bytes.Buffer
	calls := 0
	flushSnapshotAccess(ctx, time.Millisecond, func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("temporary write failure")
		}
		cancel()
		return nil
	}, &diagnostics)
	if calls != 2 || !strings.Contains(diagnostics.String(), "temporary write failure") {
		t.Fatalf("calls=%d diagnostics=%s", calls, diagnostics.String())
	}
}
