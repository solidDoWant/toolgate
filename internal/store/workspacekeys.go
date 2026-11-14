package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// WorkspaceKeyRow is an approved workspace key as the attestation check
// reads it.
type WorkspaceKeyRow struct {
	ID        int64
	Principal string
	Username  string
	Name      string
	// Status is active, replaced or revoked.
	Status    string
	PublicKey []byte
	// LastUsedAt is nil until the first attested request.
	LastUsedAt *time.Time
}

// WorkspaceKeys reads and touches workspace keys in the database. It keeps
// no cache.
type WorkspaceKeys struct {
	queries *db.Queries
}

// NewWorkspaceKeys returns a WorkspaceKeys over a pool or a transaction.
func NewWorkspaceKeys(dbtx db.DBTX) *WorkspaceKeys {
	return &WorkspaceKeys{queries: db.New(dbtx)}
}

// WorkspaceKey returns the active row of keyID, or else the newest ended
// row. The bool is false when no row has the key ID.
func (k *WorkspaceKeys) WorkspaceKey(ctx context.Context, keyID string) (WorkspaceKeyRow, bool, error) {
	row, err := k.queries.GetWorkspaceKeyByKeyID(ctx, keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceKeyRow{}, false, nil
	}

	if err != nil {
		return WorkspaceKeyRow{}, false, fmt.Errorf("get workspace key: %w", err)
	}

	key := WorkspaceKeyRow{
		ID: row.ID, Principal: row.Principal, Username: row.Username, Name: row.Name,
		Status: row.Status, PublicKey: row.PublicKey,
	}

	if row.LastUsedAt.Valid {
		used := row.LastUsedAt.Time
		key.LastUsedAt = &used
	}

	return key, true, nil
}

// HasPendingKeyRequest reports whether principal has a live key request
// for keyID.
func (k *WorkspaceKeys) HasPendingKeyRequest(ctx context.Context, principal, keyID string) (bool, error) {
	pending, err := k.queries.HasPendingKeyRequest(ctx, db.HasPendingKeyRequestParams{Principal: principal, KeyID: keyID})
	if err != nil {
		return false, fmt.Errorf("check pending key request: %w", err)
	}

	return pending, nil
}

// TouchWorkspaceKey records a use of the key. It writes at most once a
// minute per key.
func (k *WorkspaceKeys) TouchWorkspaceKey(ctx context.Context, id int64) error {
	if _, err := k.queries.TouchWorkspaceKey(ctx, id); err != nil {
		return fmt.Errorf("touch workspace key: %w", err)
	}

	return nil
}
