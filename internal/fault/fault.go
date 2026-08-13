// Package fault provides deterministic, context-scoped failure injection for
// tests of durable operation boundaries. Production callers carry no injector,
// so Check is a no-op.
package fault

import (
	"context"
	"errors"
)

// Point identifies one stable persistence or external-side-effect boundary.
type Point string

// Injector decides whether execution should fail at a named point.
type Injector interface {
	Check(Point) error
}

// InjectorFunc adapts a function to Injector.
type InjectorFunc func(Point) error

func (fn InjectorFunc) Check(point Point) error { return fn(point) }

// ErrInterrupted marks a simulated process exit. Callers must return it
// without publishing terminal operation state so a new process can reconcile
// the durable running intent.
var ErrInterrupted = errors.New("injected process interruption")

// Interrupt returns an error that models process termination at point.
func Interrupt(point Point) error { return interruption{point: point} }

type interruption struct{ point Point }

func (err interruption) Error() string { return string(err.point) + ": " + ErrInterrupted.Error() }
func (err interruption) Unwrap() error { return ErrInterrupted }

type contextKey struct{}

// WithInjector returns a child context carrying an injector. The injector is
// deliberately scoped to the call tree instead of package globals so parallel
// tests and concurrent production operations cannot affect each other.
func WithInjector(ctx context.Context, injector Injector) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if injector == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, injector)
}

// Check invokes the context injector when one is present.
func Check(ctx context.Context, point Point) error {
	if ctx == nil {
		return nil
	}
	injector, _ := ctx.Value(contextKey{}).(Injector)
	if injector == nil {
		return nil
	}
	return injector.Check(point)
}
