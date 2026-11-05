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
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("create migration lock: %w", err)
	}

	provider, closeDB, err := newProvider(pool, goose.WithSessionLocker(locker))
	if err != nil {
		return err
	}
	defer closeDB()

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	return EnsureAuditPartitions(ctx, pool, time.Now())
}

// SchemaVersions returns the highest migration version recorded in the
// database and the highest one embedded in this binary. A database version
// above the binary's means the gateway was rolled back to an older release.
func SchemaVersions(ctx context.Context, pool *pgxpool.Pool) (database, binary int64, err error) {
	provider, closeDB, err := newProvider(pool)
	if err != nil {
		return 0, 0, err
	}
	defer closeDB()

	database, binary, err = provider.GetVersions(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("read schema versions: %w", err)
	}

	return database, binary, nil
}

// newProvider returns a goose provider for the embedded migrations on pool,
// and a function that releases its database handle.
func newProvider(pool *pgxpool.Pool, options ...goose.ProviderOption) (*goose.Provider, func(), error) {
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	closeDB := func() { _ = sqlDB.Close() }

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations, options...)
	if err != nil {
		closeDB()

		return nil, nil, fmt.Errorf("create migration provider: %w", err)
	}

	return provider, closeDB, nil
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
