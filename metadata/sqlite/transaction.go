package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kumabox/kumabox/metadata"
)

// transaction restricts a callback-scoped SQL transaction to declared collections.
// Handles must not escape the Store callback or be used concurrently.
type transaction struct {
	// tx owns the snapshot and is committed or rolled back by Store.
	tx *sql.Tx
	// allowed references the store's immutable collection allowlist.
	allowed map[metadata.Collection]struct{}
	// writable denies mutations even if the driver does not enforce read-only mode.
	writable bool
}

// Get detaches driver-owned bytes and represents a missing key without an error.
func (t *transaction) Get(ctx context.Context, collection metadata.Collection, id string) ([]byte, bool, error) {
	if err := t.check(collection); err != nil {
		return nil, false, err
	}
	var data []byte
	err := t.tx.QueryRowContext(ctx, "SELECT data FROM records WHERE collection = ? AND id = ?", collection.String(), id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, mapError(err)
	}
	return append([]byte(nil), data...), true, nil
}

// Scan visits keys in SQL order, copies payloads, and closes rows on every exit.
func (t *transaction) Scan(ctx context.Context, collection metadata.Collection, visit func(string, []byte) error) (returnErr error) {
	if err := t.check(collection); err != nil {
		return err
	}
	rows, err := t.tx.QueryContext(ctx, "SELECT id, data FROM records WHERE collection = ? ORDER BY id", collection.String())
	if err != nil {
		return mapError(err)
	}
	defer func() { returnErr = errors.Join(returnErr, rows.Close()) }()
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return mapError(err)
		}
		if err := visit(id, append([]byte(nil), data...)); err != nil {
			return err
		}
	}
	return mapError(rows.Err())
}

// Put upserts a copied payload within the current writable transaction.
func (t *transaction) Put(ctx context.Context, collection metadata.Collection, id string, data []byte) error {
	if !t.writable {
		return fmt.Errorf("metadata transaction is read-only")
	}
	if err := t.check(collection); err != nil {
		return err
	}
	_, err := t.tx.ExecContext(ctx, "INSERT INTO records(collection,id,data) VALUES(?,?,?) ON CONFLICT(collection,id) DO UPDATE SET data=excluded.data", collection.String(), id, append([]byte(nil), data...))
	return mapError(err)
}

// Delete removes a key within the current writable transaction; absence is harmless.
func (t *transaction) Delete(ctx context.Context, collection metadata.Collection, id string) error {
	if !t.writable {
		return fmt.Errorf("metadata transaction is read-only")
	}
	if err := t.check(collection); err != nil {
		return err
	}
	_, err := t.tx.ExecContext(ctx, "DELETE FROM records WHERE collection = ? AND id = ?", collection.String(), id)
	return mapError(err)
}

// check rejects access outside the engine's declared collections before issuing SQL.
func (t *transaction) check(collection metadata.Collection) error {
	if _, ok := t.allowed[collection]; !ok {
		return fmt.Errorf("metadata collection %q was not declared", collection)
	}
	return nil
}
