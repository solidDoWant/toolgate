// Package store is toolgate's PostgreSQL storage: the connection pool,
// the embedded schema migrations, the generated queries (package db) and
// the change notifications every replica listens for.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open returns a connection pool for connString (a libpq URL or key/value
// string). Every new connection reads the TLS files named in connString
// (sslrootcert, sslcert, sslkey) again, so certificates rotated on disk are
// used without a restart. Connections already open keep their certificate.
func Open(ctx context.Context, connString string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL connection string: %w", err)
	}

	config.BeforeConnect = func(_ context.Context, connConfig *pgx.ConnConfig) error {
		return reloadTLS(connConfig, connString)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL pool: %w", err)
	}

	return pool, nil
}

// reloadTLS replaces the TLS settings of connConfig, a per-connection copy
// of the pool's configuration, with ones parsed from connString now.
func reloadTLS(connConfig *pgx.ConnConfig, connString string) error {
	fresh, err := pgconn.ParseConfig(connString)
	if err != nil {
		return fmt.Errorf("reload PostgreSQL TLS files: %w", err)
	}

	connConfig.TLSConfig = fresh.TLSConfig
	connConfig.Fallbacks = fresh.Fallbacks

	return nil
}
