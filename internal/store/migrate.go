package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// partitionLockKey is the advisory lock key that serializes audit
// partition creation across gateways.
const partitionLockKey = 0x746f6f6c67617465 // "toolgate"

// Migrate applies the embedded forward-only migrations, then creates the
// audit partitions for the current and the next month. Gateways starting at
// once are serialized by a PostgreSQL session advisory lock, so each
// migration is applied exactly once.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("create migration lock: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	return EnsureAuditPartitions(ctx, pool, time.Now())
}

// EnsureAuditPartitions creates the monthly audit_events partitions for the
// month of now and the month after, if they don't exist. It is safe to run
// from several gateways at once.
func EnsureAuditPartitions(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)

	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(partitionLockKey)); err != nil {
			return fmt.Errorf("lock audit partitions: %w", err)
		}

		for range 2 {
			next := month.AddDate(0, 1, 0)
			name := pgx.Identifier{"audit_events_" + month.Format("2006_01")}.Sanitize()
			// The bounds are formatted from time values, never from input.
			statement := fmt.Sprintf(
				"CREATE TABLE IF NOT EXISTS %s PARTITION OF audit_events FOR VALUES FROM ('%s') TO ('%s')",
				name, month.Format(time.RFC3339), next.Format(time.RFC3339),
			)

			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("create audit partition %s: %w", name, err)
			}

			month = next
		}

		return nil
	})
}
