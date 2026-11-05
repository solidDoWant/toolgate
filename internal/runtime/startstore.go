package runtime

import (
	"context"
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
