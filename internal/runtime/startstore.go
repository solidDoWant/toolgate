package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// StartStore is what the start path reads and writes in PostgreSQL. Every
// Mark method is a compare-and-set on the row's current status: it returns
// false, and changes nothing, when the row is no longer in that status.
type StartStore interface {
	// Session returns the session row whose identity snapshot an instance
	// is rendered with.
	Session(ctx context.Context, id int64) (db.Session, error)
	// RecordSecretsUsed stores, for a pending instance, the user secrets its
	// render used and the time each was last set.
	RecordSecretsUsed(ctx context.Context, id string, versions map[string]time.Time) error
	// MarkStarting moves a pending instance to starting and records the
	// template generation its objects were applied with and the earliest
	// expiry of its resolved values (zero for none).
	MarkStarting(ctx context.Context, id string, generation int64, resolvedExpiresAt time.Time) (bool, error)
	// MarkReady moves a starting instance to ready with its tool list.
	MarkReady(ctx context.Context, id string, tools []byte) (bool, error)
	// MarkFailed moves an instance from status from to failed.
	MarkFailed(ctx context.Context, id, from, reason string) (bool, error)
}

// PostgresStartStore is the StartStore and HealthStore on PostgreSQL. Each
// status or tool list change notifies `instance:<id>` in the same
// transaction.
type PostgresStartStore struct {
	Pool *pgxpool.Pool
}

// Session implements StartStore.
func (s PostgresStartStore) Session(ctx context.Context, id int64) (db.Session, error) {
	session, err := db.New(s.Pool).GetSessionByID(ctx, id)
	if err != nil {
		return db.Session{}, fmt.Errorf("get session %d: %w", id, err)
	}

	return session, nil
}

// RecordSecretsUsed implements StartStore.
func (s PostgresStartStore) RecordSecretsUsed(ctx context.Context, id string, versions map[string]time.Time) error {
	encoded, err := EncodeSecretsUsed(versions)
	if err != nil {
		return err
	}

	// No notification: the instance is still pending and nothing else
	// needs to wake for this write.
	if _, err := db.New(s.Pool).SetInstanceSecretsUsed(ctx, db.SetInstanceSecretsUsedParams{ID: id, SecretsUsed: encoded}); err != nil {
		return fmt.Errorf("record secrets used of instance %s: %w", id, err)
	}

	return nil
}

// MarkStarting implements StartStore.
func (s PostgresStartStore) MarkStarting(ctx context.Context, id string, generation int64, resolvedExpiresAt time.Time) (bool, error) {
	return s.change(ctx, id, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceStarting(ctx, db.SetInstanceStartingParams{
			ID: id, Generation: generation,
			ResolvedExpiresAt: pgtype.Timestamptz{Time: resolvedExpiresAt, Valid: !resolvedExpiresAt.IsZero()},
		})
	})
}

// MarkReady implements StartStore.
func (s PostgresStartStore) MarkReady(ctx context.Context, id string, tools []byte) (bool, error) {
	return s.change(ctx, id, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceReady(ctx, db.SetInstanceReadyParams{ID: id, Tools: tools})
	})
}

// MarkFailed implements StartStore.
func (s PostgresStartStore) MarkFailed(ctx context.Context, id, from, reason string) (bool, error) {
	return s.change(ctx, id, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceFailed(ctx, db.SetInstanceFailedParams{ID: id, FromStatus: from, Reason: reason})
	})
}

// Instance implements HealthStore.
func (s PostgresStartStore) Instance(ctx context.Context, id string) (db.Instance, error) {
	instance, err := db.New(s.Pool).GetInstance(ctx, id)
	if err != nil {
		return db.Instance{}, fmt.Errorf("get instance %s: %w", id, err)
	}

	return instance, nil
}

// SetTools implements HealthStore.
func (s PostgresStartStore) SetTools(ctx context.Context, id string, tools []byte) (bool, error) {
	return s.change(ctx, id, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceTools(ctx, db.SetInstanceToolsParams{ID: id, Tools: tools})
	})
}

func (s PostgresStartStore) change(ctx context.Context, id string, update func(*db.Queries) (int64, error)) (bool, error) {
	changed := false

	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := update(db.New(tx))
		if err != nil {
			return fmt.Errorf("update instance %s: %w", id, err)
		}

		if rows == 0 {
			return nil
		}

		changed = true

		return store.Notify(ctx, tx, store.KindInstance, id)
	})

	return changed, err
}

// EncodeSecretsUsed returns the stored form of the secrets a render used: a
// JSON object from secret name to the RFC 3339 time it was last set.
func EncodeSecretsUsed(versions map[string]time.Time) ([]byte, error) {
	stored := make(map[string]string, len(versions))
	for name, setAt := range versions {
		stored[name] = setAt.UTC().Format(time.RFC3339Nano)
	}

	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encode secrets used: %w", err)
	}

	return encoded, nil
}

// DecodeSecretsUsed parses the stored form written by EncodeSecretsUsed.
func DecodeSecretsUsed(encoded []byte) (map[string]time.Time, error) {
	var stored map[string]string
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return nil, fmt.Errorf("decode secrets used: %w", err)
	}

	versions := make(map[string]time.Time, len(stored))

	for name, text := range stored {
		setAt, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return nil, fmt.Errorf("decode secrets used: %w", err)
		}

		versions[name] = setAt
	}

	return versions, nil
}
