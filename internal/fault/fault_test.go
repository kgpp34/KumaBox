package fault

import (
	"errors"
	"testing"
)

func TestCheckUsesOnlyContextInjector(t *testing.T) {
	want := errors.New("injected")
	ctx := WithInjector(t.Context(), InjectorFunc(func(point Point) error {
		if point == SnapshotBeforePublish {
			return want
		}
		return nil
	}))
	if err := Check(ctx, SnapshotBeforePublish); !errors.Is(err, want) {
		t.Fatalf("Check() error = %v, want %v", err, want)
	}
	if err := Check(t.Context(), SnapshotBeforePublish); err != nil {
		t.Fatalf("plain context Check() error = %v", err)
	}
}
