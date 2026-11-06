//go:build integration

package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestMigrateCommandAppliesMigrationsAndCreatesPartitions(t *testing.T) {
	pg := testutil.StartPostgresEmpty(t)
	clearConfigEnv(t)
	t.Setenv("TOOLGATE_DATABASE_URL", pg.ConnString())

	var stdout, stderr bytes.Buffer

	require.Equal(t, 0, run([]string{"migrate"}, &stdout, &stderr), "stderr: %s", stderr.String())

	pool := openPool(t, pg.ConnString())
	database, binary, err := store.CheckSchemaVersion(t.Context(), pool)
	require.NoError(t, err)
	assert.Equal(t, binary, database)

	var partitions int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_inherits WHERE inhparent = 'audit_events'::regclass").Scan(&partitions))
	assert.GreaterOrEqual(t, partitions, 2)
}

func TestMigrateCommandExitsOneOnFailure(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("TOOLGATE_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")

	var stdout, stderr bytes.Buffer

	assert.Equal(t, 1, run([]string{"migrate"}, &stdout, &stderr))
}

func TestServeWithoutMigrateOnStartRefusesAnOlderDatabase(t *testing.T) {
	t.Run("[AC-3-15] no goose table counts as version 0 and is not created", func(t *testing.T) {
		pg := testutil.StartPostgresEmpty(t)
		pool := openPool(t, pg.ConnString())

		stdout, stderr := runServeExpectingExit(t, pg.ConnString())

		assert.Contains(t, stdout+stderr, "The database schema is at version 0. This gateway needs version ")

		var name *string
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT to_regclass('goose_db_version')::text").Scan(&name))
		assert.Nil(t, name, "serve must not create the version table")
	})

	t.Run("older database", func(t *testing.T) {
		pg := testutil.StartPostgres(t)
		pool := openPool(t, pg.ConnString())
		require.NoError(t, store.Migrate(t.Context(), pool))
		_, err := pool.Exec(t.Context(),
			"DELETE FROM goose_db_version WHERE version_id = (SELECT max(version_id) FROM goose_db_version)")
		require.NoError(t, err)

		stdout, stderr := runServeExpectingExit(t, pg.ConnString())

		assert.Contains(t, stdout+stderr, "The database schema is at version ")
		assert.Contains(t, stdout+stderr, "Run `toolgate migrate`")
	})
}

func TestServeWithoutMigrateOnStartStartsOnACurrentDatabase(t *testing.T) {
	pg := testutil.StartPostgres(t)
	require.NoError(t, store.Migrate(t.Context(), openPool(t, pg.ConnString())))

	clearConfigEnv(t)
	t.Setenv("TOOLGATE_MIGRATE_ON_START", "false")

	_, stderr, done := startServeWith(t, pg.ConnString(), testutil.StartOIDCProvider(t, testutil.OIDCOptions{}))
	stopServe(t, stderr, done)
}

// runServeExpectingExit runs serve with migrateOnStart false and returns
// its output once it exits with code 1.
func runServeExpectingExit(t *testing.T, databaseURL string) (string, string) {
	t.Helper()
	clearConfigEnv(t)

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	t.Setenv("TOOLGATE_MIGRATE_ON_START", "false")
	t.Setenv("TOOLGATE_LISTEN_ADDRESS", "127.0.0.1:0")
	t.Setenv("TOOLGATE_METRICS_LISTEN_ADDRESS", "127.0.0.1:0")
	t.Setenv("TOOLGATE_DATABASE_URL", databaseURL)
	t.Setenv("TOOLGATE_OIDC_ISSUER_URL", provider.Issuer)
	t.Setenv("TOOLGATE_OIDC_AUDIENCE", provider.Audience)

	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	code := run([]string{"serve"}, stdout, stderr)

	assert.Equal(t, 1, code)
	assert.NotContains(t, stdout.String(), `"msg":"gateway started"`)

	return stdout.String(), stderr.String()
}
