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

	if err := checkVectorVisible(ctx, pool); err != nil {
		return err
	}

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	return EnsureAuditPartitions(ctx, pool)
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

// CheckSchemaVersion fails when the database schema is older than this
// binary needs. It never changes the database: goose's GetVersions creates
// goose_db_version when it is missing, which a role with data rights only
// can't do, so the table's existence is checked first and a missing table
// counts as version 0. A database newer than the binary is returned for the
// caller to log.
func CheckSchemaVersion(ctx context.Context, pool *pgxpool.Pool) (database, binary int64, err error) {
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('goose_db_version') IS NOT NULL").Scan(&exists); err != nil {
		return 0, 0, fmt.Errorf("check for the schema version table: %w", err)
	}

	if exists {
		database, binary, err = SchemaVersions(ctx, pool)
	} else {
		binary, err = embeddedVersion(pool)
	}

	if err != nil {
		return 0, 0, err
	}

	if database < binary {
		return database, binary, &SchemaOlderError{Database: database, Binary: binary}
	}

	return database, binary, nil
}

// embeddedVersion returns the highest migration version in this binary,
// without touching the database.
func embeddedVersion(pool *pgxpool.Pool) (int64, error) {
	provider, closeDB, err := newProvider(pool)
	if err != nil {
		return 0, err
	}
	defer closeDB()

	var highest int64
	for _, source := range provider.ListSources() {
		highest = max(highest, source.Version)
	}

	return highest, nil
}

// SchemaOlderError reports a database schema older than the binary needs.
type SchemaOlderError struct {
	Database, Binary int64
}

func (e *SchemaOlderError) Error() string {
	return fmt.Sprintf("The database schema is at version %d. This gateway needs version %d. "+
		"Run `toolgate migrate`, or let the chart's migration Job run.", e.Database, e.Binary)
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

// EnsureAuditPartitions calls toolgate_ensure_audit_partitions(), which
// creates the monthly audit_events partitions for the month of the database's
// now() and the month after, if they don't exist. The function runs with its
// owner's rights, so a role with data rights only can call it, and it is safe
// to run from several gateways at once.
func EnsureAuditPartitions(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, "SELECT toolgate_ensure_audit_partitions()"); err != nil {
		return fmt.Errorf("ensure audit partitions: %w", err)
	}

	return nil
}

// CreateAuditPartitions creates the monthly audit_events partitions for the
// month of at, in UTC, and the month after, if they don't exist. It needs
// the right to create tables. Tests use it to hold rows dated in any month;
// the gateway uses EnsureAuditPartitions.
func CreateAuditPartitions(ctx context.Context, pool *pgxpool.Pool, at time.Time) error {
	month := time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)

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

// checkVectorVisible fails with a clear error when the vector extension is
// installed but its type can't be resolved on the connection's search_path.
// Without the check, the migration that uses the type fails with a bare
// "type does not exist" error. A database without the extension passes: the
// migration installs it.
func checkVectorVisible(ctx context.Context, pool *pgxpool.Pool) error {
	var (
		schema      *string
		searchPath  string
		visibleType bool
	)

	err := pool.QueryRow(ctx, `SELECT
		(SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'vector'),
		current_setting('search_path'),
		to_regtype('vector') IS NOT NULL`).Scan(&schema, &searchPath, &visibleType)
	if err != nil {
		return fmt.Errorf("check the vector extension: %w", err)
	}

	if schema == nil || visibleType {
		return nil
	}

	return fmt.Errorf("the vector type isn't visible on search_path %q; the extension is installed in schema %q. "+
		"Install it in the schema on the search_path (CREATE EXTENSION vector SCHEMA <schema>) or add %q to search_path",
		searchPath, *schema, *schema)
}
