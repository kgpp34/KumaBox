package metadata_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kumabox/kumabox/metadata"
	"github.com/kumabox/kumabox/metadata/sqlite"
)

type storeFactory func(*testing.T, []metadata.Collection) metadata.Store

func runStoreContract(t *testing.T, open storeFactory) {
	t.Helper()
	const collection metadata.Collection = "contract"
	t.Run("atomic rollback and detached bytes", func(t *testing.T) {
		store := open(t, []metadata.Collection{collection})
		ctx := t.Context()
		value := []byte("original")
		if err := store.Update(ctx, func(w metadata.Writer) error { return w.Put(ctx, collection, "a", value) }); err != nil {
			t.Fatal(err)
		}
		value[0] = 'X'
		rollback := errors.New("injected failure")
		err := store.Update(ctx, func(w metadata.Writer) error {
			if err := w.Put(ctx, collection, "a", []byte("changed")); err != nil {
				return err
			}
			if err := w.Put(ctx, collection, "b", []byte("new")); err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatalf("rollback cause = %v", err)
		}
		if err := store.View(ctx, func(r metadata.Reader) error {
			got, ok, err := r.Get(ctx, collection, "a")
			if err != nil || !ok || string(got) != "original" {
				return fmt.Errorf("get = %q, %v, %v", got, ok, err)
			}
			got[0] = 'Y'
			if err := r.Scan(ctx, collection, func(_ string, value []byte) error { value[0] = 'Z'; return nil }); err != nil {
				return err
			}
			got, _, err = r.Get(ctx, collection, "a")
			if err != nil || string(got) != "original" {
				return fmt.Errorf("detached Get = %q, %v", got, err)
			}
			_, ok, err = r.Get(ctx, collection, "b")
			if err != nil || ok {
				return fmt.Errorf("rolled-back record exists = %v, %v", ok, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("snapshot survives concurrent commit", func(t *testing.T) {
		store := open(t, []metadata.Collection{collection})
		ctx := t.Context()
		if err := store.Update(ctx, func(w metadata.Writer) error { return w.Put(ctx, collection, "key", []byte("before")) }); err != nil {
			t.Fatal(err)
		}
		if err := store.View(ctx, func(r metadata.Reader) error {
			before, _, err := r.Get(ctx, collection, "key")
			if err != nil {
				return err
			}
			if err := store.Update(ctx, func(w metadata.Writer) error { return w.Put(ctx, collection, "key", []byte("after")) }); err != nil {
				return err
			}
			after, _, err := r.Get(ctx, collection, "key")
			if err != nil || string(before) != "before" || string(after) != "before" {
				return fmt.Errorf("snapshot changed: %q -> %q, %v", before, after, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cancellation rolls back", func(t *testing.T) {
		store := open(t, []metadata.Collection{collection})
		ctx, cancel := context.WithCancel(t.Context())
		err := store.Update(ctx, func(w metadata.Writer) error {
			if err := w.Put(ctx, collection, "key", []byte("discard")); err != nil {
				return err
			}
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation = %v", err)
		}
		if err := store.View(t.Context(), func(r metadata.Reader) error {
			_, ok, err := r.Get(t.Context(), collection, "key")
			if ok {
				return errors.New("canceled transaction committed")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Update(ctx, func(metadata.Writer) error { t.Error("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled Update = %v", err)
		}
	})
	t.Run("concurrent writers and deletion", func(t *testing.T) {
		store := open(t, []metadata.Collection{collection})
		ctx := t.Context()
		var wait sync.WaitGroup
		for index := range 16 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				if err := store.Update(ctx, func(w metadata.Writer) error { return w.Put(ctx, collection, fmt.Sprint(index), []byte("value")) }); err != nil {
					t.Error(err)
				}
			}()
		}
		wait.Wait()
		if err := store.Update(ctx, func(w metadata.Writer) error { return w.Delete(ctx, collection, "0") }); err != nil {
			t.Fatal(err)
		}
		count := 0
		if err := store.View(ctx, func(r metadata.Reader) error {
			return r.Scan(ctx, collection, func(string, []byte) error { count++; return nil })
		}); err != nil {
			t.Fatal(err)
		}
		if count != 15 {
			t.Fatalf("committed record count = %d, want 15", count)
		}
	})
}

func TestStoreContract(t *testing.T) {
	factories := []struct {
		name string
		open storeFactory
	}{
		{"memory", func(t *testing.T, collections []metadata.Collection) metadata.Store {
			store, err := metadata.NewMemory(collections)
			if err != nil {
				t.Fatal(err)
			}
			return store
		}},
		{"sqlite", func(t *testing.T, collections []metadata.Collection) metadata.Store {
			store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "meta.db"), collections, sqlite.DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			return store
		}},
	}
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			runStoreContract(t, func(t *testing.T, collections []metadata.Collection) metadata.Store {
				store := factory.open(t, collections)
				t.Cleanup(func() {
					if err := store.Close(); err != nil {
						t.Error(err)
					}
				})
				return store
			})
		})
	}
}
